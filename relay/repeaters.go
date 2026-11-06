package relay

// Shared repeaters: a registry of MeshCore repeaters whose (preferably guest)
// password a host or admin has given SIERRA, so the relay's poller can log in
// through any gateway in range and read the repeater's status.
//
// Passwords are sealed with Config.SecretKey (AES-256-GCM, the repeater's
// pubkey as additional data so a sealed password can't be moved to another
// row). No API returns one; only RepeaterCredentials, for the in-process
// poller, opens them. Without a key the registry is off.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	stderrors "errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dpup/prefab/errors"
	"github.com/dpup/prefab/logging"
	"google.golang.org/grpc/codes"

	"github.com/dpup/sierra-meshrelay/meshops"
)

const repeaterSchema = `
CREATE TABLE IF NOT EXISTS repeaters (
	pubkey           TEXT PRIMARY KEY,
	name             TEXT NOT NULL DEFAULT '',
	password_sealed  BLOB NOT NULL,
	access           TEXT NOT NULL DEFAULT '',
	added_by_gateway TEXT NOT NULL DEFAULT '',
	added_by_client  TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	last_attempt_at  INTEGER NOT NULL DEFAULT 0,
	last_ok_at       INTEGER NOT NULL DEFAULT 0,
	last_gateway     TEXT NOT NULL DEFAULT '',
	last_error       TEXT NOT NULL DEFAULT ''
);
`

// RepeaterRecord is a shared repeater, without its password.
type RepeaterRecord struct {
	Pubkey string `json:"pubkey"`
	Name   string `json:"name"`
	// Access is what the password logged in as: "guest" or "admin".
	Access         string     `json:"access"`
	AddedByGateway string     `json:"added_by_gateway,omitempty"`
	AddedByClient  string     `json:"added_by_client,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	LastOKAt       *time.Time `json:"last_ok_at,omitempty"`
	LastGateway    string     `json:"last_gateway,omitempty"`
	LastError      string     `json:"last_error,omitempty"`

	sealed []byte
}

// RepeaterCredential is a shared repeater with its password, for the
// in-process poller only. Never log or serialize it.
type RepeaterCredential struct {
	RepeaterRecord
	Password string `json:"-"`
}

// ErrNoSecretKey means the registry is off: Config.SecretKey isn't set.
var ErrNoSecretKey = stderrors.New("the relay has no secret key, so it can't store repeater passwords")

const repeaterCols = `pubkey, name, password_sealed, access, added_by_gateway, added_by_client, created_at,
	updated_at, last_attempt_at, last_ok_at, last_gateway, last_error`

func scanRepeater(row interface{ Scan(...any) error }) (*RepeaterRecord, error) {
	var r RepeaterRecord
	var created, updated, attempt, ok int64
	if err := row.Scan(&r.Pubkey, &r.Name, &r.sealed, &r.Access, &r.AddedByGateway, &r.AddedByClient, &created,
		&updated, &attempt, &ok, &r.LastGateway, &r.LastError); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.CreatedAt, r.UpdatedAt = fromMS(created), fromMS(updated)
	if attempt != 0 {
		r.LastAttemptAt = timePtr(fromMS(attempt))
	}
	if ok != 0 {
		r.LastOKAt = timePtr(fromMS(ok))
	}
	return &r, nil
}

// Repeater returns one shared repeater.
func (s *Store) Repeater(pubkey string) (*RepeaterRecord, error) {
	return scanRepeater(s.db.QueryRow(`SELECT `+repeaterCols+` FROM repeaters WHERE pubkey = ?`, pubkey))
}

// Repeaters lists shared repeaters by name.
func (s *Store) Repeaters() ([]*RepeaterRecord, error) {
	rows, err := s.db.Query(`SELECT ` + repeaterCols + ` FROM repeaters ORDER BY name, pubkey`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RepeaterRecord
	for rows.Next() {
		r, err := scanRepeater(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// putRepeater inserts or replaces a repeater's password and who added it,
// keeping its poll history.
func (s *Store) putRepeater(r RepeaterRecord, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO repeaters (pubkey, name, password_sealed, access, added_by_gateway,
			added_by_client, created_at, updated_at, last_ok_at, last_gateway)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (pubkey) DO UPDATE SET name = excluded.name, password_sealed = excluded.password_sealed,
			access = excluded.access, added_by_gateway = excluded.added_by_gateway,
			added_by_client = excluded.added_by_client, updated_at = excluded.updated_at,
			last_ok_at = excluded.last_ok_at, last_gateway = excluded.last_gateway, last_error = ''`,
		r.Pubkey, r.Name, r.sealed, r.Access, r.AddedByGateway, r.AddedByClient, ms(now), ms(now), ms(now), r.LastGateway)
	return err
}

// DeleteRepeater removes a shared repeater and its password.
func (s *Store) DeleteRepeater(pubkey string) error {
	res, err := s.db.Exec(`DELETE FROM repeaters WHERE pubkey = ?`, pubkey)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordRepeaterPoll notes a poll's outcome: errText "" is a success. The
// text is shown to admins and the host who shared the repeater.
func (s *Store) RecordRepeaterPoll(pubkey, gateway string, at time.Time, errText string) error {
	if errText == "" {
		_, err := s.db.Exec(`UPDATE repeaters SET last_attempt_at = ?, last_ok_at = ?, last_gateway = ?, last_error = ''
			WHERE pubkey = ?`, ms(at), ms(at), gateway, pubkey)
		return err
	}
	_, err := s.db.Exec(`UPDATE repeaters SET last_attempt_at = ?, last_error = ? WHERE pubkey = ?`,
		ms(at), errText, pubkey)
	return err
}

// --- sealing ---------------------------------------------------------------

func (r *Relay) aead() (cipher.AEAD, error) {
	if len(r.cfg.SecretKey) == 0 {
		return nil, ErrNoSecretKey
	}
	if len(r.cfg.SecretKey) != 32 {
		return nil, fmt.Errorf("the relay's secret key must be 32 bytes, not %d", len(r.cfg.SecretKey))
	}
	b, err := aes.NewCipher(r.cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (r *Relay) seal(pubkey, password string) ([]byte, error) {
	a, err := r.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return a.Seal(nonce, nonce, []byte(password), []byte("repeater:"+pubkey)), nil
}

func (r *Relay) open(pubkey string, sealed []byte) (string, error) {
	a, err := r.aead()
	if err != nil {
		return "", err
	}
	if len(sealed) < a.NonceSize() {
		return "", stderrors.New("sealed password is too short")
	}
	pt, err := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], []byte("repeater:"+pubkey))
	if err != nil {
		return "", stderrors.New("can't open the sealed password (wrong secret key?)")
	}
	return string(pt), nil
}

// RepeaterCredentials lists shared repeaters with their passwords, for the
// in-process poller. A row whose password can't be opened is left out and
// logged (by pubkey only).
func (r *Relay) RepeaterCredentials() ([]RepeaterCredential, error) {
	recs, err := r.store.Repeaters()
	if err != nil {
		return nil, err
	}
	var out []RepeaterCredential
	for _, rec := range recs {
		pw, err := r.open(rec.Pubkey, rec.sealed)
		if err != nil {
			logging.Warnw(r.ctx, "repeater password unreadable; skipping it", "repeater", rec.Pubkey, "error", err)
			continue
		}
		out = append(out, RepeaterCredential{RepeaterRecord: *rec, Password: pw})
	}
	return out, nil
}

// RecordRepeaterPoll notes a poll's outcome for the registry (see
// Store.RecordRepeaterPoll).
func (r *Relay) RecordRepeaterPoll(pubkey, gateway string, err error) {
	text := ""
	if err != nil {
		text = RepeaterErrorText(err)
	}
	now := r.now()
	r.audit("repeater poll", func(s *Store) error { return s.RecordRepeaterPoll(pubkey, gateway, now, text) })
}

// RepeaterErrorText describes a repeater operation's failure for people.
func RepeaterErrorText(err error) string {
	var ue *UnavailableError
	var re *meshops.RefusedError
	switch {
	case err == nil:
		return ""
	case stderrors.Is(err, meshops.ErrLoginFailed):
		return "the repeater refused the password"
	case stderrors.Is(err, meshops.ErrNoMeshReply):
		return "no answer from the repeater over the mesh"
	case stderrors.Is(err, ErrBusy):
		return "the gateway's node is in use"
	case stderrors.As(err, &ue):
		return "the gateway is unavailable (" + ue.Code + ")"
	case stderrors.Is(err, ErrLeaseEnded):
		return "the gateway's node was taken over or paused mid-check"
	case stderrors.As(err, &re) && re.Code == meshops.ErrCodeTableFull:
		return "the gateway node's contacts are full"
	case stderrors.Is(err, meshops.ErrNoReply):
		return "the gateway's node didn't answer"
	}
	return "check failed"
}

// --- checking a repeater ---------------------------------------------------

// RepeaterCheck is the result of logging in to a repeater and reading its
// status through one gateway.
type RepeaterCheck struct {
	Name      string             `json:"name"`
	Access    string             `json:"access"`
	Status    *meshops.Status    `json:"status,omitempty"`
	Telemetry *meshops.Telemetry `json:"telemetry,omitempty"`
	Gateway   string             `json:"gateway"`
}

// CheckRepeater logs in to a repeater through gateway's node and reads its
// status and telemetry, under a lease at the given priority. If the node
// doesn't have the repeater as a contact, it's added for the check and
// removed after, so the host's contacts are left as they were. name is used
// for an added contact; "" keeps the node's own.
func (r *Relay) CheckRepeater(ctx context.Context, gateway, client string, priority int, pubkey, name, password string) (RepeaterCheck, error) {
	res := RepeaterCheck{Gateway: gateway, Name: name}
	err := r.Use(ctx, LeaseRequest{Gateway: gateway, Client: client, Role: RoleAgent, Priority: priority},
		func(ctx context.Context, s *meshops.Session) error {
			if _, err := s.AppStart(ctx, "sierra-relay"); err != nil {
				return err
			}
			// A node whose clock went backwards can't log in (replay
			// protection); companion apps set the clock on connect too.
			_, _ = s.SyncClock(ctx, 5*time.Minute)
			c, err := s.Contact(ctx, pubkey)
			switch {
			case stderrors.Is(err, meshops.ErrNotFound):
				if err := s.AddContact(ctx, meshops.Contact{Pubkey: pubkey, Type: meshops.TypeRepeater, Name: name}); err != nil {
					return err
				}
				defer func() {
					cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					_ = s.RemoveContact(cctx, pubkey)
				}()
			case err != nil:
				return err
			default:
				if c.Name != "" {
					res.Name = c.Name
				}
			}
			login, err := s.Login(ctx, pubkey, password)
			if err != nil {
				return err
			}
			res.Access = login.Access()
			st, err := s.Status(ctx, pubkey)
			if err != nil {
				return err
			}
			res.Status = &st
			if tm, err := s.Telemetry(ctx, pubkey); err == nil {
				res.Telemetry = &tm
			}
			return nil
		})
	return res, err
}

// --- API ---------------------------------------------------------------------

// gatewayHandler is a bridge self-service handler with the authenticated
// gateway.
type gatewayHandler func(req *http.Request, gw *GatewayRecord) (any, error)

// bridgeAuthed authenticates a bridge token (smb_…), counting failures
// toward the IP lockout like every other credential.
func (r *Relay) bridgeAuthed(h gatewayHandler) func(*http.Request) (any, error) {
	return func(req *http.Request) (any, error) {
		ip := r.clientIP(req)
		if r.lockout.isLocked(ip, r.now()) {
			return nil, errors.NewC("too many authentication failures", codes.ResourceExhausted)
		}
		tok := bearerToken(req.Header.Get("Authorization"))
		var rec *GatewayRecord
		err := ErrNotFound
		if strings.HasPrefix(tok, BridgeTokenPrefix) {
			rec, err = r.store.GatewayByToken(TokenHash(tok))
		}
		if stderrors.Is(err, ErrNotFound) {
			r.m.authFailures.Add(1)
			r.lockout.fail(ip, r.now())
			logging.Infow(r.ctx, "auth failed", "kind", "gateway-api", "ip", ip, "path", req.URL.Path)
			return nil, errors.NewC("a bridge token is required", codes.Unauthenticated)
		}
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
		if req.Method != http.MethodGet {
			logging.Infow(r.ctx, "gateway action", "gateway", rec.Name, "method", req.Method, "path", req.URL.Path)
		}
		return h(req, rec)
	}
}

// NearbyRepeater is a repeater in a gateway node's contacts.
type NearbyRepeater struct {
	Pubkey     string     `json:"pubkey"`
	Name       string     `json:"name"`
	LastAdvert *time.Time `json:"last_advert,omitempty"`
	Shared     bool       `json:"shared"`
}

func (r *Relay) apiGatewayNearbyRepeaters(req *http.Request, gw *GatewayRecord) (any, error) {
	var contacts []meshops.Contact
	err := r.Use(req.Context(), LeaseRequest{Gateway: gw.Name, Client: "repeater-check", Role: RoleAgent, Priority: PriorityNormal},
		func(ctx context.Context, s *meshops.Session) error {
			if _, err := s.AppStart(ctx, "sierra-relay"); err != nil {
				return err
			}
			var err error
			contacts, err = s.Contacts(ctx)
			return err
		})
	if err != nil {
		return nil, nodeUseErr(err)
	}
	shared := map[string]bool{}
	if recs, err := r.store.Repeaters(); err == nil {
		for _, rec := range recs {
			shared[rec.Pubkey] = true
		}
	}
	out := []NearbyRepeater{}
	for _, c := range contacts {
		if c.Type != meshops.TypeRepeater {
			continue
		}
		n := NearbyRepeater{Pubkey: c.Pubkey, Name: c.Name, Shared: shared[c.Pubkey]}
		if !c.LastAdvert.IsZero() {
			n.LastAdvert = timePtr(c.LastAdvert)
		}
		out = append(out, n)
	}
	return map[string]any{"repeaters": out}, nil
}

// nodeUseErr turns a failure to use a gateway's node into an API error.
func nodeUseErr(err error) error {
	var ue *UnavailableError
	switch {
	case stderrors.Is(err, ErrBusy):
		return conflict("the node is in use by someone on SIERRA right now; try again in a few minutes")
	case stderrors.As(err, &ue):
		return conflict("the gateway's node isn't available (" + ue.Code + ")")
	case stderrors.Is(err, ErrLeaseEnded):
		return conflict("the node was taken over or paused during the check; try again")
	case stderrors.Is(err, meshops.ErrNoReply):
		return errors.NewC("the node didn't answer", codes.Unavailable).WithHTTPStatusCode(http.StatusBadGateway)
	}
	return errors.Wrap(err, 0)
}

// repeaterErr turns a check's failure into an API error.
func repeaterErr(err error) error {
	switch {
	case stderrors.Is(err, meshops.ErrLoginFailed):
		return errors.NewC("the repeater refused that password", codes.PermissionDenied)
	case stderrors.Is(err, meshops.ErrNoMeshReply):
		return errors.NewC("the repeater didn't answer over the mesh; check it's on and in range, then try again",
			codes.DeadlineExceeded).WithHTTPStatusCode(http.StatusGatewayTimeout)
	}
	var re *meshops.RefusedError
	if stderrors.As(err, &re) && re.Code == meshops.ErrCodeTableFull {
		return conflict("the node's contacts are full; remove one and try again")
	}
	return nodeUseErr(err)
}

type addRepeaterBody struct {
	Pubkey   string `json:"pubkey"`
	Name     string `json:"name,omitempty"`
	Password string `json:"password"`
	// Gateway (admin only) is the gateway to check the password through.
	Gateway string `json:"gateway,omitempty"`
}

// addRepeater checks the password through gateway, then stores it.
func (r *Relay) addRepeater(ctx context.Context, body addRepeaterBody, gateway, byGateway, byClient string) (any, error) {
	pk, ok := NormalizePubkey(body.Pubkey)
	if !ok {
		return nil, errors.NewC("pubkey must be 64 hex characters", codes.InvalidArgument)
	}
	if len(body.Password) > 15 {
		// MeshCore passwords are at most 15 characters.
		return nil, errors.NewC("that's longer than a MeshCore password can be (15 characters)", codes.InvalidArgument)
	}
	if _, err := r.aead(); err != nil {
		return nil, errors.NewC(err.Error(), codes.Unavailable).WithHTTPStatusCode(http.StatusServiceUnavailable)
	}
	if existing, err := r.store.Repeater(pk); err == nil && byGateway != "" && existing.AddedByGateway != byGateway {
		return nil, conflict("this repeater is already shared with SIERRA by someone else")
	}
	check, err := r.CheckRepeater(ctx, gateway, "repeater-check", PriorityNormal, pk, strings.TrimSpace(body.Name), body.Password)
	if err != nil {
		return nil, repeaterErr(err)
	}
	sealed, err := r.seal(pk, body.Password)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	rec := RepeaterRecord{Pubkey: pk, Name: check.Name, Access: check.Access, AddedByGateway: byGateway,
		AddedByClient: byClient, LastGateway: gateway, sealed: sealed}
	if err := r.store.putRepeater(rec, r.now()); err != nil {
		return nil, errors.Wrap(err, 0)
	}
	logging.Infow(r.ctx, "repeater shared", "repeater", pk, "name", check.Name, "access", check.Access,
		"by_gateway", byGateway, "by_client", byClient, "checked_via", gateway)
	saved, err := r.store.Repeater(pk)
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	return map[string]any{"repeater": saved, "check": check}, nil
}

func (r *Relay) apiGatewayAddRepeater(req *http.Request, gw *GatewayRecord) (any, error) {
	var body addRepeaterBody
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	if body.Gateway != "" && body.Gateway != gw.Name {
		return nil, errors.NewC("a bridge can only check repeaters through its own node", codes.PermissionDenied)
	}
	return r.addRepeater(req.Context(), body, gw.Name, gw.Name, "")
}

func (r *Relay) apiGatewayListRepeaters(_ *http.Request, gw *GatewayRecord) (any, error) {
	recs, err := r.store.Repeaters()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	mine := []*RepeaterRecord{}
	for _, rec := range recs {
		if rec.AddedByGateway == gw.Name {
			mine = append(mine, rec)
		}
	}
	return map[string]any{"repeaters": mine}, nil
}

func (r *Relay) apiGatewayRemoveRepeater(req *http.Request, gw *GatewayRecord) (any, error) {
	pk, _ := NormalizePubkey(req.PathValue("pubkey"))
	rec, err := r.store.Repeater(pk)
	if err != nil {
		return nil, storeErr(err, "no such shared repeater")
	}
	if rec.AddedByGateway != gw.Name {
		return nil, errors.NewC("only the gateway that shared a repeater (or an admin) can remove it", codes.PermissionDenied)
	}
	if err := r.store.DeleteRepeater(pk); err != nil {
		return nil, storeErr(err, "no such shared repeater")
	}
	logging.Infow(r.ctx, "repeater removed", "repeater", pk, "by_gateway", gw.Name)
	return map[string]any{"removed": pk}, nil
}

func (r *Relay) apiListRepeaters(_ *http.Request, _ *ClientRecord) (any, error) {
	recs, err := r.store.Repeaters()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}
	if recs == nil {
		recs = []*RepeaterRecord{}
	}
	return map[string]any{"repeaters": recs}, nil
}

func (r *Relay) apiAddRepeater(req *http.Request, who *ClientRecord) (any, error) {
	var body addRepeaterBody
	if err := decodeBody(req, &body); err != nil {
		return nil, err
	}
	if body.Gateway == "" {
		return nil, errors.NewC("gateway is required: the password is checked through that gateway's node", codes.InvalidArgument)
	}
	return r.addRepeater(req.Context(), body, body.Gateway, "", who.Name)
}

func (r *Relay) apiRemoveRepeater(req *http.Request, who *ClientRecord) (any, error) {
	pk, _ := NormalizePubkey(req.PathValue("pubkey"))
	if err := r.store.DeleteRepeater(pk); err != nil {
		return nil, storeErr(err, "no such shared repeater")
	}
	logging.Infow(r.ctx, "repeater removed", "repeater", pk, "by_client", who.Name)
	return map[string]any{"removed": pk}, nil
}
