// Package message is the structured form of everything a bot says, rendered
// separately for each channel (Matrix HTML, Telegram HTML, plain text).
package message

import (
	"html"
	"strings"
)

// Message is one bot message. Every field is plain text; renderers escape it.
//
//	🔴 **[host] 告警** (after)
//	line 1
//	line 2
//	┌pre──────┐
//	└─────────┘
//	_note_
type Message struct {
	Icon  string // emoji in front of the title
	Title string // shown bold
	After string // shown right after the title, not bold
	Lines []string
	Pre   string // preformatted block (tables)
	Note  string // italic footer
}

// WithNote returns a copy with a footer note appended.
func (m Message) WithNote(note string) Message {
	if m.Note != "" {
		m.Note += "\n" + note
	} else {
		m.Note = note
	}
	return m
}

func (m Message) head() string {
	if m.Icon == "" {
		return m.Title
	}
	return m.Icon + " " + m.Title
}

// Plain renders the message without markup (Matrix "body").
func (m Message) Plain() string {
	var parts []string
	if h := m.head() + m.After; h != "" {
		parts = append(parts, h)
	}
	parts = append(parts, m.Lines...)
	if m.Pre != "" {
		parts = append(parts, m.Pre)
	}
	if m.Note != "" {
		parts = append(parts, m.Note)
	}
	return strings.Join(parts, "\n")
}

// render builds the HTML both channels share; only the line break differs.
// Telegram's HTML keeps "\n" as is; Matrix needs <br>. Inside <pre> both keep "\n".
func (m Message) render(br string) string {
	var parts []string
	var head string
	if m.Icon != "" {
		head = html.EscapeString(m.Icon) + " "
	}
	if m.Title != "" {
		head += "<b>" + html.EscapeString(m.Title) + "</b>"
	}
	head += html.EscapeString(m.After)
	if head != "" {
		parts = append(parts, head)
	}
	for _, l := range m.Lines {
		parts = append(parts, html.EscapeString(l))
	}
	out := strings.Join(parts, br)
	if m.Pre != "" {
		if out != "" {
			out += br
		}
		out += "<pre>" + html.EscapeString(m.Pre) + "</pre>"
	}
	if m.Note != "" {
		if out != "" {
			out += br
		}
		note := strings.ReplaceAll(html.EscapeString(m.Note), "\n", br)
		out += "<i>" + note + "</i>"
	}
	return out
}

// TelegramHTML renders for Telegram's parse_mode=HTML.
func (m Message) TelegramHTML() string { return m.render("\n") }

// MatrixHTML renders org.matrix.custom.html for formatted_body.
func (m Message) MatrixHTML() string { return m.render("<br>") }
