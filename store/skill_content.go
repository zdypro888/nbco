package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const SkillContentVersion = 1

var ErrInvalidSkillContent = errors.New("invalid skill content")

// SkillContent is the durable machine contract for a reusable procedure.
// Human-facing labels are rendered only at presentation boundaries.
type SkillContent struct {
	Version     int    `json:"version"`
	Trigger     string `json:"trigger"`
	Summary     string `json:"summary"`
	Procedure   string `json:"procedure"`
	Constraints string `json:"constraints"`
}

func NewSkillContent(trigger, summary, procedure, constraints string) SkillContent {
	return SkillContent{
		Version:     SkillContentVersion,
		Trigger:     strings.TrimSpace(trigger),
		Summary:     strings.TrimSpace(summary),
		Procedure:   strings.TrimSpace(procedure),
		Constraints: strings.TrimSpace(constraints),
	}
}

func (c SkillContent) Validate() error {
	if c.Version != SkillContentVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalidSkillContent, c.Version)
	}
	if strings.TrimSpace(c.Trigger) == "" || strings.TrimSpace(c.Summary) == "" || strings.TrimSpace(c.Procedure) == "" {
		return fmt.Errorf("%w: trigger, summary, and procedure are required", ErrInvalidSkillContent)
	}
	return nil
}

func EncodeSkillContent(content SkillContent) (string, error) {
	content = NewSkillContent(content.Trigger, content.Summary, content.Procedure, content.Constraints)
	if err := content.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode skill content: %w", err)
	}
	return string(raw), nil
}

func DecodeSkillContent(raw string) (SkillContent, error) {
	var content SkillContent
	dec := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&content); err != nil {
		return SkillContent{}, fmt.Errorf("%w: %v", ErrInvalidSkillContent, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return SkillContent{}, err
	}
	content.Trigger = strings.TrimSpace(content.Trigger)
	content.Summary = strings.TrimSpace(content.Summary)
	content.Procedure = strings.TrimSpace(content.Procedure)
	content.Constraints = strings.TrimSpace(content.Constraints)
	if err := content.Validate(); err != nil {
		return SkillContent{}, err
	}
	return content, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrInvalidSkillContent)
		}
		return fmt.Errorf("%w: trailing data: %v", ErrInvalidSkillContent, err)
	}
	return nil
}

// RenderSkillContent turns the typed contract into readable instructions.
// Nothing parses this representation back into state.
func RenderSkillContent(content SkillContent) string {
	var b strings.Builder
	fmt.Fprintf(&b, "触发条件：%s\n", content.Trigger)
	fmt.Fprintf(&b, "摘要：%s\n", content.Summary)
	fmt.Fprintf(&b, "执行方法：\n%s", content.Procedure)
	if content.Constraints != "" {
		fmt.Fprintf(&b, "\n限制与禁忌：\n%s", content.Constraints)
	}
	return b.String()
}

func SkillSearchText(content SkillContent) string {
	return strings.Join([]string{content.Trigger, content.Summary, content.Procedure, content.Constraints}, "\n")
}
