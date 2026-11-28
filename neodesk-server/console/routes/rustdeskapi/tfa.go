package rustdeskapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/neodeskapp/neodesk/neodesk-server/cluster"
)

// tfaSession is a sign-in that waits for its second factor, with the
// client id and UUID of the first step ("" where the body had no text).
type tfaSession struct {
	userID     int64
	username   string
	role       string
	clientID   string
	clientUUID string
	createdAt  time.Time
}

// tfaStored is a tfaSession as the KV holds it.
type tfaStored struct {
	UserID     int64     `json:"user_id"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	ClientID   string    `json:"client_id"`
	ClientUUID string    `json:"client_uuid"`
	CreatedAt  time.Time `json:"created_at"`
}

// tfaSessions holds the pending second steps of one listener
// (req.app.locals._tfaSessions) in a KV: a sign-in started on one port is
// finished on the same port, on any instance that shares the KV
// (kit.Deps.KV). Without a shared KV each listener keeps its own in
// memory. A step is good for five minutes: an older one is refused when
// it is used, and the KV drops it after that time.
type tfaSessions struct {
	kv cluster.KV
}

// newTFASessions keeps the pending steps in kv, or in memory when kv is
// nil.
func newTFASessions(kv cluster.KV) *tfaSessions {
	if kv == nil {
		kv = cluster.NewMemKV()
	}
	return &tfaSessions{kv: kv}
}

const tfaMaxAge = 5 * time.Minute

func tfaKey(secret string) string { return "console:rdtfa:" + secret }

// add stores a pending second step under its secret for five minutes.
func (s *tfaSessions) add(secret string, session tfaSession) error {
	raw, err := json.Marshal(tfaStored{
		UserID: session.userID, Username: session.username, Role: session.role,
		ClientID: session.clientID, ClientUUID: session.clientUUID, CreatedAt: session.createdAt,
	})
	if err != nil {
		return err
	}
	return s.kv.Set(context.Background(), tfaKey(secret), string(raw), tfaMaxAge)
}

// get returns the pending second step of a secret that is not older than
// five minutes at now. A secret that is not text matches none.
func (s *tfaSessions) get(secret any, now time.Time) (tfaSession, bool, error) {
	text, ok := secret.(string)
	if !ok {
		return tfaSession{}, false, nil
	}
	raw, found, err := s.kv.Get(context.Background(), tfaKey(text))
	if err != nil || !found {
		return tfaSession{}, false, err
	}
	var st tfaStored
	if json.Unmarshal([]byte(raw), &st) != nil {
		return tfaSession{}, false, nil
	}
	if now.Sub(st.CreatedAt) > tfaMaxAge {
		_ = s.kv.Del(context.Background(), tfaKey(text))
		return tfaSession{}, false, nil
	}
	return tfaSession{
		userID: st.UserID, username: st.Username, role: st.Role,
		clientID: st.ClientID, clientUUID: st.ClientUUID, createdAt: st.CreatedAt,
	}, true, nil
}

// remove drops a pending second step once it is used. It is false when
// the step was gone already: another request used it first.
func (s *tfaSessions) remove(secret string) (bool, error) {
	_, ok, err := s.kv.Take(context.Background(), tfaKey(secret))
	return ok, err
}
