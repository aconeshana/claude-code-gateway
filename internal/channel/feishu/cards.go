package feishu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

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
		// P1 trailing layout: markdown left + single button right via a
		// 2-column split. Skips the regular markdown + action rendering.
		if sec.ButtonLayout == "trailing" && sec.Markdown != "" && len(sec.Buttons) == 1 {
			elements = append(elements, trailingButtonRow(sec.Markdown, sec.Buttons[0]))
			if sec.Note != "" {
				elements = append(elements, noteElement(sec.Note))
			}
			if sec.Form != nil {
				elements = append(elements, formElement(*sec.Form))
			}
			continue
		}
		if sec.Markdown != "" {
			elements = append(elements, markdownElement(convertToLarkMD(sec.Markdown)))
		}
		if sec.Note != "" {
			elements = append(elements, noteElement(sec.Note))
		}
		if len(sec.Buttons) > 0 {
			if sec.ButtonLayout == "fill" && len(sec.Buttons) > 1 {
				// P0 fill layout: each button gets its own equal-weight column,
				// width="fill" so the row spans the card edge-to-edge.
				elements = append(elements, fillButtonsRow(sec.Buttons))
			} else {
				actions := make([]interface{}, 0, len(sec.Buttons))
				for _, b := range sec.Buttons {
					actions = append(actions, buttonElement(b))
				}
				elements = append(elements, actionElement(actions))
			}
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
	// Use a custom encoder with SetEscapeHTML(false) so & in form-submit
	// button names (querystring separators) survives as a literal byte
	// rather than &. Lark accepts both, but the literal form is
	// readable in logs and avoids decode complications downstream.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(card)
	// Encoder.Encode appends a trailing newline; strip it to match prior behavior.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return string(out)
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
	// Build the form's inner elements. If LeadingButtons are present, layout
	// them with the input(s) and submit button in a single column_set so
	// everything renders on one line (typical use: [Detail][Input][Submit]).
	formElems := make([]interface{}, 0, len(f.Fields)+2)

	inputElems := make([]interface{}, 0, len(f.Fields)+1)
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
		inputElems = append(inputElems, input)
	}
	var submitBtn map[string]interface{}
	if f.Submit.Label != "" {
		submitBtn = buttonElement(f.Submit)
		submitBtn["form_action_type"] = "submit"
		// Lark form_submit swallows button.value, so all button-level
		// payload must be smuggled through button.name. We encode the
		// entire Action map as a querystring (action=X&behavior=allow&
		// source=local&...) so callers can pass arbitrary keys without
		// hitting the historical 2-field limit ("<action>:<key>") that
		// dropped extras silently. Decode side: channel.go's fallback.
		submitBtn["name"] = encodeSubmitName(f.Submit.Action, f.FormID)
	}

	if len(f.LeadingButtons) > 0 {
		// Inline layout: column_set with [LeadingBtns...] [Inputs...] [Submit].
		// Lark's column_set rejects the `action` container — buttons must be
		// placed directly as column elements. Additionally, any interactive
		// element inside a form (including non-submit buttons) requires a
		// unique `name` field, so we synthesize one from FormID+index.
		cols := make([]interface{}, 0, len(f.LeadingButtons)+len(inputElems)+1)
		for i, b := range f.LeadingButtons {
			btn := buttonElement(b)
			btn["name"] = fmt.Sprintf("%s_lead_%d", f.FormID, i)
			cols = append(cols, columnWith("weighted", 1, []interface{}{btn}))
		}
		for _, ie := range inputElems {
			cols = append(cols, columnWith("weighted", 3, []interface{}{ie}))
		}
		if submitBtn != nil {
			cols = append(cols, columnWith("weighted", 1, []interface{}{submitBtn}))
		}
		formElems = append(formElems, columnSet(cols))
	} else {
		formElems = append(formElems, inputElems...)
		// P2: when SecondaryButtons exist, lay submit + secondary side-by-side
		// in a left-aligned column_set so [保存][取消] sit next to each other
		// instead of stacking. Buttons use width=auto so they hug their text.
		if submitBtn != nil && len(f.SecondaryButtons) > 0 {
			cols := make([]interface{}, 0, len(f.SecondaryButtons)+1)
			cols = append(cols, columnWith("auto", 0, []interface{}{submitBtn}))
			for i, b := range f.SecondaryButtons {
				btn := buttonElement(b)
				btn["name"] = fmt.Sprintf("%s_sec_%d", f.FormID, i)
				cols = append(cols, columnWith("auto", 0, []interface{}{btn}))
			}
			cs := columnSet(cols)
			cs["horizontal_align"] = "left"
			formElems = append(formElems, cs)
		} else if submitBtn != nil {
			formElems = append(formElems, submitBtn)
		}
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

func columnSet(cols []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"tag":                "column_set",
		"flex_mode":          "none",
		"background_style":   "default",
		"horizontal_spacing": "small",
		"columns":            cols,
	}
}

func columnWith(width string, weight int, elements []interface{}) map[string]interface{} {
	col := map[string]interface{}{
		"tag":            "column",
		"width":          width,
		"vertical_align": "center",
		"elements":       elements,
	}
	if width == "weighted" {
		col["weight"] = weight
	}
	return col
}

// fillButtonsRow lays N buttons evenly across the card width — each gets its
// own equal-weight column and the button itself stretches with width="fill".
// flex_mode=bisect on N=2 makes the split visually balanced.
func fillButtonsRow(buttons []channel.Button) map[string]interface{} {
	cols := make([]interface{}, 0, len(buttons))
	for _, b := range buttons {
		btn := buttonElement(b)
		btn["width"] = "fill"
		col := map[string]interface{}{
			"tag":              "column",
			"width":            "weighted",
			"weight":           1,
			"vertical_align":   "center",
			"horizontal_align": "center",
			"elements":         []interface{}{btn},
		}
		cols = append(cols, col)
	}
	cs := map[string]interface{}{
		"tag":     "column_set",
		"columns": cols,
	}
	if len(buttons) == 2 {
		cs["flex_mode"] = "bisect"
	}
	return cs
}

// trailingButtonRow renders markdown on the left + a single button on the
// right. Card row height shrinks because the button stops occupying its own
// line. Used by archived list rows where there's one primary action.
func trailingButtonRow(md string, b channel.Button) map[string]interface{} {
	cols := []interface{}{
		columnWith("weighted", 5, []interface{}{markdownElement(convertToLarkMD(md))}),
		columnWith("auto", 0, []interface{}{buttonElement(b)}),
	}
	return columnSet(cols)
}

// encodeSubmitName packs an action map into a string suitable for the
// form-submit button's `name` field. Lark form_submit drops button.value
// entirely, so this is the only channel for button-level metadata —
// without it, form callbacks can only carry the submit handler's
// identity ("which button") and form_value ("what the user typed"),
// losing per-form context (e.g. which behavior/source the wizard had
// preselected before reaching the input step).
//
// Format:
//   - querystring: key1=value1&key2=value2 (keys sorted for determinism)
//   - empty action map → falls back to "<formID>_submit" sentinel so the
//     callback still has something to match in dispatcher logs
//
// The "=" character is the format discriminator: channel.go's decode
// uses its presence to distinguish this form from the legacy
// "<action>:<key>" two-field encoding (still emitted by older callers
// not yet migrated).
func encodeSubmitName(action map[string]string, formID string) string {
	if len(action) == 0 {
		if formID == "" {
			return "submit"
		}
		return formID + "_submit"
	}
	keys := make([]string, 0, len(action))
	for k := range action {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(action[k]))
	}
	return strings.Join(parts, "&")
}
