package dingtalk

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// renderCard converts a channel.Card into DingTalk actionCard JSON string.
// Buttons are rendered inline with their corresponding content sections
// (each section with buttons becomes its own markdown block + button row).
func renderCard(c channel.Card) string {
	// Build markdown body with inline button labels after each section.
	body := renderCardMarkdownWithButtons(c)

	// Collect all buttons for the actionCard btns array.
	btns := renderAllButtons(c)

	card := map[string]interface{}{
		"msgtype": "actionCard",
		"actionCard": map[string]interface{}{
			"title":          c.Title,
			"text":           body,
			"btnOrientation": "0", // 0=vertical so buttons stack with labels
		},
	}
	if len(btns) > 0 {
		card["actionCard"].(map[string]interface{})["btns"] = btns
	}

	data, _ := json.Marshal(card)
	return string(data)
}

// renderCardMarkdownWithButtons builds the card body markdown.
// DingTalk actionCard puts all buttons at the bottom, so we don't add inline
// button markers — the buttons themselves carry descriptive labels.
func renderCardMarkdownWithButtons(c channel.Card) string {
	var parts []string

	for _, sec := range c.Sections {
		if sec.Divider {
			parts = append(parts, "---")
			parts = append(parts, "")
		}
		if sec.Markdown != "" {
			parts = append(parts, convertToDingTalkMD(sec.Markdown))
			parts = append(parts, "")
		}
		if sec.Note != "" {
			parts = append(parts, "> "+sec.Note)
			parts = append(parts, "")
		}
	}

	body := strings.TrimSpace(strings.Join(parts, "\n"))
	if body == "" && c.Title != "" {
		body = "**" + c.Title + "**"
	}
	return body
}

// renderCardMarkdown assembles all card sections into markdown (no button labels).
// Used for cards sent as pure markdown messages (no buttons).
func renderCardMarkdown(c channel.Card) string {
	var parts []string

	if c.Title != "" {
		parts = append(parts, "## "+c.Title)
		parts = append(parts, "")
	}

	for _, sec := range c.Sections {
		if sec.Divider {
			parts = append(parts, "---")
			parts = append(parts, "")
		}
		if sec.Markdown != "" {
			parts = append(parts, convertToDingTalkMD(sec.Markdown))
			parts = append(parts, "")
		}
		if sec.Note != "" {
			parts = append(parts, "> "+sec.Note)
			parts = append(parts, "")
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// renderAllButtons collects all buttons from all sections. Each button's
// actionURL makes the client send a recognizable text message that
// onChatBotMessage intercepts as a card action.
// Since DingTalk renders all buttons at the bottom (separated from content),
// we embed the section's first line into the button label so users can
// identify which item the button belongs to.
func renderAllButtons(c channel.Card) []map[string]interface{} {
	var btns []map[string]interface{}
	for _, sec := range c.Sections {
		if len(sec.Buttons) == 0 {
			continue
		}
		// Extract a short context hint from the section's markdown.
		hint := sectionHint(sec.Markdown)

		for _, b := range sec.Buttons {
			actionData, _ := json.Marshal(b.Action)
			content := cardActionPrefix + string(actionData)
			actionURL := "dtmd://dingtalkclient/sendMessage?content=" + url.QueryEscape(content)

			label := b.Label
			if hint != "" {
				label = b.Label + " | " + hint
			}
			btn := map[string]interface{}{
				"title":     label,
				"actionURL": actionURL,
			}
			btns = append(btns, btn)
		}
	}
	return btns
}

// sectionHint extracts the first meaningful line from section markdown,
// truncated to ~20 runes. Used to label buttons so users can identify them.
func sectionHint(md string) string {
	if md == "" {
		return ""
	}
	lines := strings.Split(md, "\n")
	// Take the first non-empty line.
	var first string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" {
			first = t
			break
		}
	}
	if first == "" {
		return ""
	}
	// Strip markdown formatting for a cleaner label.
	first = strings.ReplaceAll(first, "**", "")
	first = strings.ReplaceAll(first, "`", "")
	// Truncate.
	runes := []rune(first)
	if len(runes) > 25 {
		return string(runes[:25]) + "…"
	}
	return first
}

// renderCallbackCard renders a card for DingTalk's interactive card API
// with callback support (used by UpdateMessage and card callback responses).
func renderCallbackCard(c channel.Card) map[string]interface{} {
	var elements []interface{}

	for _, sec := range c.Sections {
		if sec.Divider {
			elements = append(elements, map[string]interface{}{
				"tag": "hr",
			})
		}
		if sec.Markdown != "" {
			elements = append(elements, map[string]interface{}{
				"tag":  "markdown",
				"text": convertToDingTalkMD(sec.Markdown),
			})
		}
		if sec.Note != "" {
			elements = append(elements, map[string]interface{}{
				"tag":  "markdown",
				"text": "> " + sec.Note,
			})
		}
		if len(sec.Buttons) > 0 {
			var btnElements []interface{}
			for _, b := range sec.Buttons {
				actionData, _ := json.Marshal(b.Action)
				btnElements = append(btnElements, map[string]interface{}{
					"tag":   "button",
					"text":  b.Label,
					"style": buttonStyle(b.Style),
					"actionData": map[string]interface{}{
						"cardPrivateData": map[string]interface{}{
							"params": b.Action,
						},
					},
					"callbackData": string(actionData),
				})
			}
			elements = append(elements, map[string]interface{}{
				"tag":     "action",
				"actions": btnElements,
			})
		}
	}

	return map[string]interface{}{
		"config": map[string]interface{}{
			"autoLayout": true,
		},
		"header": map[string]interface{}{
			"title": map[string]interface{}{
				"tag":  "plain_text",
				"text": c.Title,
			},
			"style": toneToHeaderStyle(c.Tone),
		},
		"body": map[string]interface{}{
			"elements": elements,
		},
	}
}

func toneToHeaderStyle(t channel.Tone) string {
	switch t {
	case channel.ToneSuccess:
		return "GREEN"
	case channel.ToneWarning:
		return "ORANGE"
	case channel.ToneError:
		return "RED"
	case channel.ToneInfo:
		return "BLUE"
	case channel.ToneNeutral, "":
		return "GREY"
	default:
		return "GREY"
	}
}

func buttonStyle(style string) string {
	switch style {
	case "primary":
		return "PRIMARY"
	case "danger":
		return "DANGER"
	default:
		return "DEFAULT"
	}
}
