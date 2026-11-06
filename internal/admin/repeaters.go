package admin

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// checkTimeout bounds a request that logs in to a repeater over the mesh.
const checkTimeout = 3 * time.Minute

// Repeater is a repeater shared with SIERRA (never its password).
type Repeater struct {
	Pubkey         string     `json:"pubkey"`
	Name           string     `json:"name"`
	Access         string     `json:"access"`
	AddedByGateway string     `json:"added_by_gateway"`
	AddedByClient  string     `json:"added_by_client"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAttemptAt  *time.Time `json:"last_attempt_at"`
	LastOKAt       *time.Time `json:"last_ok_at"`
	LastGateway    string     `json:"last_gateway"`
	LastError      string     `json:"last_error"`
}

// NearbyRepeater is a repeater in the bridge's node's contacts.
type NearbyRepeater struct {
	Pubkey     string     `json:"pubkey"`
	Name       string     `json:"name"`
	LastAdvert *time.Time `json:"last_advert"`
	Shared     bool       `json:"shared"`
}

// RepeaterCheck is what the relay read from a repeater while checking its
// password.
type RepeaterCheck struct {
	Name   string `json:"name"`
	Access string `json:"access"`
	Status *struct {
		BatteryMilliVolts *int   `json:"battery_mv"`
		UptimeS           *int64 `json:"uptime_s"`
	} `json:"status"`
	Gateway string `json:"gateway"`
}

// SharedRepeater is the answer to sharing a repeater.
type SharedRepeater struct {
	Repeater Repeater      `json:"repeater"`
	Check    RepeaterCheck `json:"check"`
}

func (c *Client) slow() *Client {
	cc := *c
	if cc.HTTP == nil {
		cc.HTTP = &http.Client{Timeout: checkTimeout}
	}
	return &cc
}

// NearbyRepeaters lists the repeaters the bridge's node knows (bridge token).
func (c *Client) NearbyRepeaters(ctx context.Context) ([]NearbyRepeater, error) {
	var out struct {
		Repeaters []NearbyRepeater `json:"repeaters"`
	}
	return out.Repeaters, c.do(ctx, http.MethodGet, "/gateway/repeaters/nearby", nil, &out)
}

// MyRepeaters lists the repeaters this bridge shared (bridge token).
func (c *Client) MyRepeaters(ctx context.Context) ([]Repeater, error) {
	var out struct {
		Repeaters []Repeater `json:"repeaters"`
	}
	return out.Repeaters, c.do(ctx, http.MethodGet, "/gateway/repeaters", nil, &out)
}

// ShareRepeater checks password through the bridge's node, then shares the
// repeater (bridge token).
func (c *Client) ShareRepeater(ctx context.Context, pubkey, name, password string) (SharedRepeater, error) {
	var out SharedRepeater
	body := map[string]string{"pubkey": pubkey, "name": name, "password": password}
	return out, c.slow().do(ctx, http.MethodPost, "/gateway/repeaters", body, &out)
}

// UnshareRepeater removes a repeater this bridge shared (bridge token).
func (c *Client) UnshareRepeater(ctx context.Context, pubkey string) error {
	return c.do(ctx, http.MethodDelete, "/gateway/repeaters/"+url.PathEscape(pubkey), nil, nil)
}

// Repeaters lists every shared repeater (admin).
func (c *Client) Repeaters(ctx context.Context) ([]Repeater, error) {
	var out struct {
		Repeaters []Repeater `json:"repeaters"`
	}
	return out.Repeaters, c.do(ctx, http.MethodGet, "/repeaters", nil, &out)
}

// AddRepeater checks password through gateway's node, then shares the
// repeater (admin).
func (c *Client) AddRepeater(ctx context.Context, pubkey, gateway, password string) (SharedRepeater, error) {
	var out SharedRepeater
	body := map[string]string{"pubkey": pubkey, "gateway": gateway, "password": password}
	return out, c.slow().do(ctx, http.MethodPost, "/repeaters", body, &out)
}

// RemoveRepeater removes any shared repeater (admin).
func (c *Client) RemoveRepeater(ctx context.Context, pubkey string) error {
	return c.do(ctx, http.MethodDelete, "/repeaters/"+url.PathEscape(pubkey), nil, nil)
}
