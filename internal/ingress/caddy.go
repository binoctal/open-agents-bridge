// Package ingress wires public hostnames to app ports through the Caddy
// admin API (127.0.0.1:2019, localhost-only by design -- the admin endpoint
// must never be reachable off-host).
//
// Ported verbatim from node-runner's internal/ingress (design D2: after
// node-runner retires, hosting-agent is the admin API's only caller; the
// wildcard-placeholder ordering comments below are load-bearing field
// knowledge from the v0 implementation).
package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Caddy manages one route per deployment under the server's inbound route
// set, keyed by route @id "oa-{deploymentID}" so upserts are idempotent.
type Caddy struct {
	// Addr is the admin endpoint (default 127.0.0.1:2019).
	Addr string
	// DomainSuffix builds the host matcher: {label}.DomainSuffix.
	DomainSuffix string

	HTTP *http.Client
}

// NewCaddy builds an ingress writer.
func NewCaddy(addr, domainSuffix string) *Caddy {
	return &Caddy{
		Addr:         addr,
		DomainSuffix: domainSuffix,
		HTTP:         &http.Client{Timeout: 10 * time.Second},
	}
}

// routeID is the stable Caddy route @id for one deployment.
func routeID(deploymentID string) string { return "oa-" + deploymentID }

// Host returns the public hostname for a deployment.
func (c *Caddy) Host(deploymentID string) string {
	return deploymentID + "." + c.DomainSuffix
}

// Upsert ensures a route {label}.suffix -> {containerAddr} exists.
//
// Two call shapes: PATCH /id/{id} replaces a route already carrying the @id
// in place (404 when new); for a new route, PUT to the list path at index 0
// INSERTS before everything else. Ordering is load-bearing: the Caddyfile's
// wildcard placeholder (*.suffix, terminal) shadows any host it matches, and
// POST-append lands after it -- a dynamic route appended there can never
// fire.
func (c *Caddy) Upsert(ctx context.Context, deploymentID, containerAddr string) error {
	route := map[string]any{
		"@id": routeID(deploymentID),
		"match": []any{map[string]any{
			"host": []string{c.Host(deploymentID)},
		}},
		"handle": []any{map[string]any{
			"handler": "reverse_proxy",
			"upstreams": []any{map[string]any{
				"dial": containerAddr,
			}},
		}},
	}
	// Replace-in-place first: PATCH /id/{id} 404s when the route is new.
	err := c.doRoute(ctx, http.MethodPatch,
		fmt.Sprintf("http://%s/id/%s", c.Addr, routeID(deploymentID)), route)
	if err == nil || !isNotFound(err) {
		return err
	}
	// New route: insert at the head of the list (must precede the wildcard
	// placeholder route compiled from the Caddyfile).
	return c.doRoute(ctx, http.MethodPut,
		fmt.Sprintf("http://%s/config/apps/http/servers/srv0/routes/0", c.Addr), route)
}

// notFoundError marks a 404 from the admin API so callers can branch on it.
type notFoundError struct{ underlying error }

func (e *notFoundError) Error() string { return e.underlying.Error() }
func (e *notFoundError) Unwrap() error { return e.underlying }

func isNotFound(err error) bool {
	var nf *notFoundError
	return errors.As(err, &nf)
}

// Remove deletes the route (absence is success).
func (c *Caddy) Remove(ctx context.Context, deploymentID string) error {
	err := c.doRoute(ctx, http.MethodDelete,
		fmt.Sprintf("http://%s/id/%s", c.Addr, routeID(deploymentID)), nil)
	if err == nil || isNotFound(err) {
		return nil
	}
	return err
}

// doRoute performs one admin-API call. A nil body means no body (DELETE).
// The server name in a stock Caddyfile is "srv0"; deployments route under it.
// If the host uses a different server name the first call 404s and the error
// names the path, which is enough to diagnose from logs.
func (c *Caddy) doRoute(ctx context.Context, method, path string, route map[string]any) error {
	var reader io.Reader
	if route != nil {
		body, err := json.Marshal(route)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return err
	}
	if route != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("caddy %s route: %w", strings.ToLower(method), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return &notFoundError{underlying: fmt.Errorf("caddy %s route: status 404", strings.ToLower(method))}
	}
	var sb strings.Builder
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	sb.Write(buf[:n])
	return fmt.Errorf("caddy %s route: status %d: %s", strings.ToLower(method), resp.StatusCode, sb.String())
}
