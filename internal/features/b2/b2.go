// Package b2 reports how much Backblaze B2 storage each bucket uses. It
// lists file versions with the native B2 API (stdlib HTTP) on a schedule
// and answers from the last listing: listing calls are billed per 1000
// files returned (class C), so commands never trigger one.
package b2

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/htarkx/Matrix-Bot-Go/internal/bot"
	"github.com/htarkx/Matrix-Bot-Go/internal/config"
	"github.com/htarkx/Matrix-Bot-Go/internal/human"
	"github.com/htarkx/Matrix-Bot-Go/internal/message"
)

// DefaultAuthURL is the B2 account authorization endpoint.
const DefaultAuthURL = "https://api.backblazeb2.com/b2api/v3/b2_authorize_account"

// Config is the module's JSON configuration.
type Config struct {
	// Credentials holding the application key ID and the key. The key may
	// be read-only; it needs listBuckets and listFiles. Every bucket it can
	// see is reported.
	KeyIDCredential string `json:"key_id_credential"` // default "b2_key_id"
	KeyCredential   string `json:"key_credential"`    // default "b2_key"
	// Refresh is how often the buckets are listed (default 1h).
	Refresh config.Duration `json:"refresh"`
	// AuthURL overrides DefaultAuthURL (tests).
	AuthURL string `json:"auth_url"`
}

// Bucket is one bucket's usage.
type Bucket struct {
	Name         string
	Files        int   // current, visible files
	Bytes        int64 // their size
	Versions     int   // every stored version, current ones included
	VersionBytes int64 // their size: what B2 bills
	Hidden       int   // file names whose newest version is a hide marker
}

// snapshot is one completed listing.
type snapshot struct {
	at      time.Time
	buckets []Bucket
}

// Module is the b2 module.
type Module struct {
	bot.NoChecks
	cfg  Config
	env  bot.Env
	http *http.Client

	last    atomic.Pointer[snapshot]
	lastErr atomic.Pointer[error]
}

// New is the module factory.
func New(env bot.Env, raw json.RawMessage) (bot.Module, error) {
	var c Config
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
	}
	if c.KeyIDCredential == "" {
		c.KeyIDCredential = "b2_key_id"
	}
	if c.KeyCredential == "" {
		c.KeyCredential = "b2_key"
	}
	if c.Refresh.Duration == 0 {
		c.Refresh.Duration = time.Hour
	}
	if c.Refresh.Duration < 10*time.Minute {
		return nil, errors.New("refresh below 10m: every listing is billed")
	}
	if c.AuthURL == "" {
		c.AuthURL = DefaultAuthURL
	}
	return &Module{cfg: c, env: env, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

// Jobs implements bot.Module.
func (m *Module) Jobs() []bot.Job {
	return []bot.Job{{Name: "b2-usage", Every: m.cfg.Refresh.Duration, Delay: 20 * time.Second, Run: m.refresh}}
}

// Commands implements bot.Module.
func (m *Module) Commands() []bot.Command {
	return []bot.Command{{Name: "buckets", Aliases: []string{"b"}, Help: "B2 存储用量（每个桶）", Run: m.cmdBuckets}}
}

func (m *Module) refresh(ctx context.Context) error {
	buckets, err := m.list(ctx)
	if err != nil {
		m.lastErr.Store(&err)
		return err
	}
	m.lastErr.Store(nil)
	m.last.Store(&snapshot{at: time.Now(), buckets: buckets})
	return nil
}

// list authorizes and lists every visible bucket's file versions.
func (m *Module) list(ctx context.Context) ([]Bucket, error) {
	keyID, err := m.env.Secret(m.cfg.KeyIDCredential)
	if err != nil {
		return nil, err
	}
	key, err := m.env.Secret(m.cfg.KeyCredential)
	if err != nil {
		return nil, err
	}
	if keyID == "" || key == "" {
		return nil, errors.New("no B2 key (dry run?)")
	}
	var auth struct {
		AccountID string `json:"accountId"`
		Token     string `json:"authorizationToken"`
		APIInfo   struct {
			StorageAPI struct {
				APIURL string `json:"apiUrl"`
				// Set when the key is restricted to one bucket (v3 puts it
				// right here); such a key may not call b2_list_buckets
				// without naming the bucket.
				BucketID   *string `json:"bucketId"`
				BucketName *string `json:"bucketName"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.AuthURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(keyID, key)
	if err := m.do(req, &auth); err != nil {
		return nil, fmt.Errorf("authorize: %w", err)
	}
	api := strings.TrimRight(auth.APIInfo.StorageAPI.APIURL, "/") + "/b2api/v3/"
	call := func(name string, body, out any) error {
		b, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+name, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", auth.Token)
		req.Header.Set("Content-Type", "application/json")
		if err := m.do(req, out); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}

	var lb struct {
		Buckets []struct {
			ID   string `json:"bucketId"`
			Name string `json:"bucketName"`
		} `json:"buckets"`
	}
	if sa := auth.APIInfo.StorageAPI; sa.BucketID != nil && *sa.BucketID != "" {
		name := *sa.BucketID
		if sa.BucketName != nil {
			name = *sa.BucketName
		}
		lb.Buckets = append(lb.Buckets, struct {
			ID   string `json:"bucketId"`
			Name string `json:"bucketName"`
		}{*sa.BucketID, name})
	} else if err := call("b2_list_buckets", map[string]string{"accountId": auth.AccountID}, &lb); err != nil {
		return nil, err
	}
	var out []Bucket
	for _, b := range lb.Buckets {
		u := Bucket{Name: b.Name}
		// Versions come sorted by name, newest first within a name.
		body := map[string]any{"bucketId": b.ID, "maxFileCount": 10000}
		prevName := "\x00"
		for {
			var page struct {
				Files []struct {
					Name   string `json:"fileName"`
					Size   int64  `json:"contentLength"`
					Action string `json:"action"`
				} `json:"files"`
				NextFileName *string `json:"nextFileName"`
				NextFileID   *string `json:"nextFileId"`
			}
			if err := call("b2_list_file_versions", body, &page); err != nil {
				return nil, fmt.Errorf("bucket %s: %w", b.Name, err)
			}
			for _, f := range page.Files {
				newest := f.Name != prevName
				prevName = f.Name
				switch f.Action {
				case "upload":
					u.Versions++
					u.VersionBytes += f.Size
					if newest {
						u.Files++
						u.Bytes += f.Size
					}
				case "hide":
					if newest {
						u.Hidden++
					}
				}
			}
			if page.NextFileName == nil || page.NextFileID == nil {
				break
			}
			body["startFileName"], body["startFileId"] = *page.NextFileName, *page.NextFileID
		}
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Bucket) int { return cmp.Compare(b.VersionBytes, a.VersionBytes) })
	return out, nil
}

func (m *Module) do(req *http.Request, out any) error {
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &e) == nil && e.Code != "" {
			if e.Message == "" {
				return fmt.Errorf("HTTP %d %s", resp.StatusCode, e.Code)
			}
			return fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, e.Code, e.Message)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// state is the last listing, or a line saying why there is none.
func (m *Module) state() (*snapshot, string) {
	s := m.last.Load()
	if s != nil {
		return s, ""
	}
	if e := m.lastErr.Load(); e != nil {
		return nil, "读取失败：" + (*e).Error()
	}
	return nil, "统计中"
}

// Summary implements bot.Summarizer.
func (m *Module) Summary(context.Context) ([]string, error) {
	s, why := m.state()
	if s == nil {
		return []string{"存储  B2 " + why}, nil
	}
	var total int64
	for _, b := range s.buckets {
		total += b.VersionBytes
	}
	lines := []string{fmt.Sprintf("存储  B2 共 %s（%d 个桶，%s前统计）", human.Bytes(total), len(s.buckets), human.Duration(time.Since(s.at)))}
	for _, b := range s.buckets {
		l := fmt.Sprintf("      %s %s", b.Name, human.Bytes(b.Bytes))
		if old := b.VersionBytes - b.Bytes; old > 0 {
			l += "（旧版本另占 " + human.Bytes(old) + "）"
		}
		lines = append(lines, l)
	}
	return lines, nil
}

func (m *Module) cmdBuckets(context.Context, bot.Request) (message.Message, error) {
	msg := message.Message{Icon: "🪣", Title: "[" + m.env.Host + "] B2 存储"}
	s, why := m.state()
	if s == nil {
		msg.After = "：" + why
		return msg, nil
	}
	var rows []string
	for _, b := range s.buckets {
		rows = append(rows,
			b.Name,
			fmt.Sprintf("  当前 %d 个文件 %s", b.Files, human.Bytes(b.Bytes)),
			fmt.Sprintf("  计费 %d 个版本 %s", b.Versions, human.Bytes(b.VersionBytes)))
		if b.Hidden > 0 {
			rows = append(rows, fmt.Sprintf("  已隐藏 %d 个（等待生命周期删除）", b.Hidden))
		}
	}
	msg.Pre = strings.Join(rows, "\n")
	msg.Note = fmt.Sprintf("%s前统计，每 %s统计一次；计费按所有版本算", human.Duration(time.Since(s.at)), human.Duration(m.cfg.Refresh.Duration))
	if e := m.lastErr.Load(); e != nil {
		msg.Note += "\n上次刷新失败：" + (*e).Error()
	}
	return msg, nil
}
