package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	eventsModule "github.com/neodeskapp/neodesk/neodesk-server/events"
)

// TestASavedSettingIsAnnouncedToTheOtherInstances: a settings route that
// succeeds publishes what changed on the settings bus; one that fails
// does not.
func TestASavedSettingIsAnnouncedToTheOtherInstances(t *testing.T) {
	srv := &Server{}
	bus := eventsModule.NewBus()
	srv.SetSettingsBus(bus)
	sub := bus.Subscribe(eventsModule.EventSettingsChanged)
	status := http.StatusOK
	h := srv.broadcasts(eventsModule.SettingsLDAP, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})

	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/api/auth/ldap/config", nil))
	select {
	case ev := <-sub.Ch:
		if ev.Data["what"] != eventsModule.SettingsLDAP {
			t.Fatalf("announced %v", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("a saved setting was not announced")
	}

	status = http.StatusBadRequest
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/api/auth/ldap/config", nil))
	select {
	case ev := <-sub.Ch:
		t.Fatalf("a refused save was announced: %v", ev.Data)
	case <-time.After(100 * time.Millisecond):
	}
}
