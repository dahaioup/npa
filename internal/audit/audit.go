package audit

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/npa/npa/internal/model"
	"github.com/npa/npa/internal/policy"
)

type Repository interface {
	AddAudit(event model.AuditEvent) (model.AuditEvent, error)
	ListAudit(limit, offset int) ([]model.AuditEvent, error)
	PruneAudit(before time.Time) (int, error)
}

type Logger struct {
	repo Repository
}

func New(repo Repository) *Logger { return &Logger{repo: repo} }

var sensitive = regexp.MustCompile(`(?i)(password|passwd|secret|token|authorization|private[_ -]?key)\s*[:=]\s*([^\s,;]+)`)

func Redact(value string) string {
	return sensitive.ReplaceAllString(value, "$1=[REDACTED]")
}

func (l *Logger) Record(ctx context.Context, operation, nodeID string, capability policy.Capability, params string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	event := model.AuditEvent{
		Time:       time.Now().UTC(),
		Source:     string(policy.SourceFrom(ctx)),
		Operation:  operation,
		NodeID:     nodeID,
		Capability: capability.String(),
		Parameters: Redact(params),
		Result:     result,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if saved, saveErr := l.repo.AddAudit(event); saveErr == nil {
		event = saved
	}
	attrs := []any{
		"source", event.Source, "operation", event.Operation, "node_id", event.NodeID,
		"capability", event.Capability, "result", event.Result, "duration_ms", event.DurationMS,
	}
	if err != nil {
		slog.Error("operation failed", append(attrs, "error", Redact(err.Error()))...)
		return
	}
	slog.Info("operation completed", attrs...)
}

func (l *Logger) List(limit, offset int) ([]model.AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return l.repo.ListAudit(limit, offset)
}

func (l *Logger) Prune(retentionDays int) error {
	if retentionDays <= 0 {
		return nil
	}
	_, err := l.repo.PruneAudit(time.Now().AddDate(0, 0, -retentionDays))
	return err
}

func SafeParameters(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for key, value := range values {
		parts = append(parts, key+"="+Redact(strings.TrimSpace(value)))
	}
	return strings.Join(parts, ",")
}
