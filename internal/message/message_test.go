package message

import "testing"

func TestRender(t *testing.T) {
	m := Message{
		Icon:  "📊",
		Title: "[h] 状态",
		After: "，1 条告警中",
		Lines: []string{"a < b", "c & d"},
		Pre:   "x  1\ny  2",
		Note:  "from <telegram>",
	}
	cases := []struct {
		name, got, want string
	}{
		{"plain", m.Plain(),
			"📊 [h] 状态，1 条告警中\na < b\nc & d\nx  1\ny  2\nfrom <telegram>"},
		{"telegram", m.TelegramHTML(),
			"📊 <b>[h] 状态</b>，1 条告警中\na &lt; b\nc &amp; d\n<pre>x  1\ny  2</pre>\n<i>from &lt;telegram&gt;</i>"},
		{"matrix", m.MatrixHTML(),
			"📊 <b>[h] 状态</b>，1 条告警中<br>a &lt; b<br>c &amp; d<br><pre>x  1\ny  2</pre><br><i>from &lt;telegram&gt;</i>"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

func TestRenderSparse(t *testing.T) {
	if got := (Message{Lines: []string{"only"}}).MatrixHTML(); got != "only" {
		t.Errorf("lines only: %q", got)
	}
	if got := (Message{Pre: "p"}).TelegramHTML(); got != "<pre>p</pre>" {
		t.Errorf("pre only: %q", got)
	}
	m := Message{Title: "t"}.WithNote("one").WithNote("two")
	if got := m.MatrixHTML(); got != "<b>t</b><br><i>one<br>two</i>" {
		t.Errorf("notes: %q", got)
	}
}
