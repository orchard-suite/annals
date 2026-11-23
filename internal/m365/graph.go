// Package m365 collects Microsoft 365 audit records into Annals: who
// shared what with whom in SharePoint and OneDrive, and each Copilot prompt
// with the tools it set off and the files it touched. It reads the unified
// audit log through Microsoft Graph's audit log query API, signed in as its
// own app registration with a federated workload token (no secret), and
// posts each record as events under the source its Annals token allows.
package m365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/orchard-suite/annals/internal/graph"
)

// Graph reads the audit log through a Graph client.
type Graph struct{ c *graph.Client }

func NewGraph(c *graph.Client) *Graph { return &Graph{c: c} }

// Record is one unified audit log record as Graph answers it. AuditData is
// the workload's own record, which differs by record type.
type Record struct {
	ID                string          `json:"id"`
	CreatedDateTime   time.Time       `json:"createdDateTime"`
	RecordType        string          `json:"auditLogRecordType"`
	Operation         string          `json:"operation"`
	Service           string          `json:"service"`
	UserID            string          `json:"userId"`
	UserPrincipalName string          `json:"userPrincipalName"`
	UserType          string          `json:"userType"`
	ObjectID          string          `json:"objectId"`
	AuditData         json.RawMessage `json:"auditData"`
}

// RecordTypes are the audit record types the collector reads.
var RecordTypes = []string{"sharePointSharingOperation", "copilotInteraction"}

// ErrQueryFailed is a query Graph gave up on.
var ErrQueryFailed = errors.New("the audit log query failed")

// Records runs one audit log query over [from, to) for the record types and
// calls each with every record it found, a page at a time. Queries run on
// Graph's side for minutes; it polls until the query is done or ctx ends.
func (g *Graph) Records(ctx context.Context, from, to time.Time, poll time.Duration, each func([]Record) error) error {
	var q struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	err := g.c.Do(ctx, http.MethodPost, "/security/auditLog/queries", map[string]any{
		"@odata.type":         "#microsoft.graph.security.auditLogQuery",
		"displayName":         "annals " + from.UTC().Format(time.RFC3339) + " to " + to.UTC().Format(time.RFC3339),
		"filterStartDateTime": from.UTC().Format(time.RFC3339),
		"filterEndDateTime":   to.UTC().Format(time.RFC3339),
		"recordTypeFilters":   RecordTypes,
	}, &q)
	if err != nil {
		return fmt.Errorf("start the query: %w", err)
	}
	for q.Status != "succeeded" {
		switch q.Status {
		case "failed", "cancelled":
			return fmt.Errorf("%w: %s", ErrQueryFailed, q.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
		if err := g.c.Do(ctx, http.MethodGet, "/security/auditLog/queries/"+url.PathEscape(q.ID), nil, &q); err != nil {
			return fmt.Errorf("read the query: %w", err)
		}
	}
	next := "/security/auditLog/queries/" + url.PathEscape(q.ID) + "/records?$top=1000"
	for next != "" {
		var page struct {
			Value []Record `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if err := g.c.Do(ctx, http.MethodGet, next, nil, &page); err != nil {
			return fmt.Errorf("read the records: %w", err)
		}
		if err := each(page.Value); err != nil {
			return err
		}
		next = page.Next
	}
	return nil
}
