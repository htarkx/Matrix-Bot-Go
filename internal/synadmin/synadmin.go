// Package synadmin reads the Synapse admin API, which Tuwunel serves too.
// The caller must be a server admin.
package synadmin

import (
	"context"
	"net/url"
	"strconv"

	"github.com/htarkx/Matrix-Bot-Go/internal/matrix"
)

// Getter is the read side of a Matrix identity (matrix.Identity.Get).
type Getter interface {
	Get(ctx context.Context, path string, query url.Values, out any) error
}

var _ Getter = (*matrix.Identity)(nil)

const pageSize = 100

// maxPages bounds every listing: a runaway pagination must not hang a command.
const maxPages = 100

// User is one local account.
type User struct {
	Name        string `json:"name"` // full user ID
	DisplayName string `json:"displayname"`
	Admin       bool   `json:"admin"`
	Deactivated bool   `json:"deactivated"`
	LastSeenTS  int64  `json:"last_seen_ts"` // ms; 0 = never
}

// Users lists the active local accounts.
func Users(ctx context.Context, g Getter) ([]User, error) {
	var all []User
	from := ""
	for range maxPages {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if from != "" {
			q.Set("from", from)
		}
		var page struct {
			Users     []User `json:"users"`
			NextToken any    `json:"next_token"` // string in Synapse, number in some servers
		}
		if err := g.Get(ctx, "/_synapse/admin/v2/users", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Users...)
		from = token(page.NextToken)
		if from == "" || len(page.Users) == 0 {
			break
		}
	}
	return all, nil
}

// Room is one room the server knows.
type Room struct {
	ID             string  `json:"room_id"`
	Name           *string `json:"name"`
	CanonicalAlias *string `json:"canonical_alias"`
	JoinedMembers  int     `json:"joined_members"`
	Creator        string  `json:"creator"`
	Encryption     *string `json:"encryption"`
}

// Rooms lists every room.
func Rooms(ctx context.Context, g Getter) ([]Room, error) {
	var all []Room
	from := ""
	for range maxPages {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if from != "" {
			q.Set("from", from)
		}
		var page struct {
			Rooms     []Room `json:"rooms"`
			NextBatch any    `json:"next_batch"`
		}
		if err := g.Get(ctx, "/_synapse/admin/v1/rooms", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Rooms...)
		from = token(page.NextBatch)
		if from == "" || len(page.Rooms) == 0 {
			break
		}
	}
	return all, nil
}

// Members returns a room's joined members.
func Members(ctx context.Context, g Getter, roomID string) ([]string, error) {
	var out struct {
		Members []string `json:"members"`
	}
	err := g.Get(ctx, "/_synapse/admin/v1/rooms/"+url.PathEscape(roomID)+"/members", nil, &out)
	return out.Members, err
}

// UserMedia is one user's uploads.
type UserMedia struct {
	UserID string `json:"user_id"`
	Count  int    `json:"media_count"`
	Bytes  int64  `json:"media_length"`
}

// MediaStats lists uploads per local user, as the homeserver accounts them.
func MediaStats(ctx context.Context, g Getter) ([]UserMedia, error) {
	var all []UserMedia
	from := 0
	for range maxPages {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "from": {strconv.Itoa(from)}}
		var page struct {
			Users     []UserMedia `json:"users"`
			NextToken any         `json:"next_token"`
		}
		if err := g.Get(ctx, "/_synapse/admin/v1/statistics/users/media", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Users...)
		next, err := strconv.Atoi(token(page.NextToken))
		if err != nil || len(page.Users) == 0 {
			break
		}
		from = next
	}
	return all, nil
}

// token normalises a pagination token that may be a string, a number or null.
func token(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	default:
		return ""
	}
}

// DisplayName is a room's name for people: its name, else its alias, else "".
func (r Room) DisplayName() string {
	if r.Name != nil && *r.Name != "" {
		return *r.Name
	}
	if r.CanonicalAlias != nil && *r.CanonicalAlias != "" {
		return *r.CanonicalAlias
	}
	return ""
}
