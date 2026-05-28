package feishu

import (
	"encoding/json"

	"github.com/anthropics/claude-code-gateway/internal/channel"
)

// renderCard converts a channel.Card into the Lark interactive card JSON
// format (returned as a string ready to pass to the Lark Message Create API).
func renderCard(c channel.Card) string {
	template := toneToTemplate(c.Tone)

	elements := []interface{}{}
	for _, sec := range c.Sections {
		if sec.Divider {
			elements = append(elements, dividerElement())
		}
		if sec.Markdown != "" {
			elements = append(elements, markdownElement(convertToLarkMD(sec.Markdown)))
		}
		if sec.Note != "" {
			elements = append(elements, noteElement(sec.Note))
		}
		if len(sec.Buttons) > 0 {
			actions := make([]interface{}, 0, len(sec.Buttons))
			for _, b := range sec.Buttons {
				actions = append(actions, buttonElement(b))
			}
			elements = append(elements, actionElement(actions))
		}
		if sec.Form != nil {
			elements = append(elements, formElement(*sec.Form))
		}
	}

	card := map[string]interface{}{
		"config": map[string]interface{}{"wide_screen_mode": true},
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"content": c.Title, "tag": "plain_text"},
			"template": template,
		},
		"elements": elements,
	}
	data, _ := json.Marshal(card)
	return string(data)
}

func toneToTemplate(t channel.Tone) string {
	switch t {
	case channel.ToneSuccess:
		return "green"
	case channel.ToneWarning:
		return "orange"
	case channel.ToneError:
		return "red"
	case channel.ToneInfo:
		return "blue"
	case channel.ToneNeutral, "":
		return "grey"
	default:
		return string(t)
	}
}

func markdownElement(content string) map[string]interface{} {
	return map[string]interface{}{
		"tag":  "div",
		"text": map[string]interface{}{"tag": "lark_md", "content": content},
	}
}

func dividerElement() map[string]interface{} {
	return map[string]interface{}{"tag": "hr"}
}

func noteElement(text string) map[string]interface{} {
	return map[string]interface{}{
		"tag": "note",
		"elements": []interface{}{
			map[string]interface{}{"tag": "lark_md", "content": text},
		},
	}
}

func actionElement(actions []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":     "action",
		"actions": actions,
	}
}

func buttonElement(b channel.Button) map[string]interface{} {
	btnType := b.Style
	if btnType == "" {
		btnType = "default"
	}
	value := make(map[string]interface{}, len(b.Action))
	for k, v := range b.Action {
		value[k] = v
	}
	return map[string]interface{}{
		"tag":   "button",
		"text":  map[string]interface{}{"tag": "plain_text", "content": b.Label},
		"type":  btnType,
		"value": value,
	}
}

func formElement(f channel.Form) map[string]interface{} {
	formElems := make([]interface{}, 0, len(f.Fields)+1)
	for _, field := range f.Fields {
		input := map[string]interface{}{
			"tag":  "input",
			"name": field.Name,
			"placeholder": map[string]interface{}{
				"tag":     "plain_text",
				"content": field.Placeholder,
			},
		}
		if field.Initial != "" {
			input["default_value"] = field.Initial
		}
		if field.Label != "" {
			input["label"] = map[string]interface{}{"tag": "plain_text", "content": field.Label}
		}
		formElems = append(formElems, input)
	}
	if f.Submit.Label != "" {
		submitBtn := buttonElement(f.Submit)
		submitBtn["form_action_type"] = "submit"
		// Encode the action+key into the button name. Lark's form submit
		// event does NOT carry the button.value map — only form_value plus
		// the button.name. We encode "<action>:<key>" so the receiver can
		// route the submission back to the right handler.
		actionName, _ := f.Submit.Action["action"]
		key, _ := f.Submit.Action["key"]
		if actionName != "" {
			submitBtn["name"] = actionName + ":" + key
		} else {
			submitBtn["name"] = f.FormID + "_submit"
		}
		formElems = append(formElems, submitBtn)
	}
	formName := f.FormID
	if formName == "" {
		formName = "form"
	}
	return map[string]interface{}{
		"tag":      "form",
		"name":     formName,
		"elements": formElems,
	}
}
