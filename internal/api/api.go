// Package api is viiwork-parrot's loopback HTTP API, used by the CLI and viiwork.
package api

import (
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/janit/viiwork-parrot/internal/node"
	"github.com/janit/viiwork-parrot/internal/schedule"
	"github.com/janit/viiwork-parrot/internal/units"
)

type Backend interface {
	Status() []node.ModelStatus
	Ensure(id string) (node.ModelStatus, error)
	Limits() node.LimitsStatus
	SetOverride(schedule.Partial)
	ClearOverride()
}

type EnsureRequest struct {
	ID string `json:"id"`
}

type EnsureResponse struct {
	Path   string           `json:"path,omitempty"`
	Error  string           `json:"error,omitempty"`
	Status node.ModelStatus `json:"status"`
}

type OverrideRequest struct {
	Upload   *string `json:"upload,omitempty"`
	Download *string `json:"download,omitempty"`
	MaxConns *int    `json:"max_conns,omitempty"`
}

func Handler(b Backend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, b.Status())
	})
	mux.HandleFunc("POST /ensure", func(w http.ResponseWriter, r *http.Request) {
		var req EnsureRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
			writeJSON(w, http.StatusBadRequest, EnsureResponse{Error: "body must be {\"id\": \"<model id>\"}"})
			return
		}
		st, err := b.Ensure(req.ID)
		switch {
		case errors.Is(err, node.ErrUnknownModel):
			writeJSON(w, http.StatusNotFound, EnsureResponse{Error: err.Error()})
		case errors.Is(err, node.ErrNoCatalog):
			writeJSON(w, http.StatusServiceUnavailable, EnsureResponse{Error: err.Error()})
		case errors.Is(err, node.ErrNoSpace):
			writeJSON(w, http.StatusInsufficientStorage, EnsureResponse{Error: st.Error, Status: st})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, EnsureResponse{Error: err.Error(), Status: st})
		case st.State == node.StateSeeding:
			writeJSON(w, http.StatusOK, EnsureResponse{Path: st.Path, Status: st})
		case st.State == node.StateFailed:
			writeJSON(w, http.StatusInternalServerError, EnsureResponse{Error: st.Error, Status: st})
		case st.State == node.StateAbsent:
			// seed_only_existing and no local copy: waiting would be forever.
			msg := st.Error
			if msg == "" {
				msg = "model is not on this host and this node never downloads (models.seed_only_existing)"
			}
			writeJSON(w, http.StatusConflict, EnsureResponse{Error: msg, Status: st})
		default:
			writeJSON(w, http.StatusAccepted, EnsureResponse{Status: st})
		}
	})
	mux.HandleFunc("GET /limits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, b.Limits())
	})
	mux.HandleFunc("PUT /limits/override", func(w http.ResponseWriter, r *http.Request) {
		var req OverrideRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p, err := req.partial()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.SetOverride(p)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /limits/override", func(w http.ResponseWriter, r *http.Request) {
		b.ClearOverride()
		w.WriteHeader(http.StatusNoContent)
	})
	return guard(mux)
}

// guard is the API's browser/DNS-rebinding hardening (it has no auth and
// listens on loopback only):
//   - Host must be a loopback IP or "localhost": a page on a rebound
//     domain reaches us with its own name as Host (421).
//   - POST/PUT/DELETE must carry Content-Type: application/json: that makes
//     every cross-origin browser request a preflighted one, which we never
//     answer (415).
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "Host must be a loopback address or localhost", http.StatusMisdirectedRequest)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (o OverrideRequest) partial() (schedule.Partial, error) {
	var p schedule.Partial
	for _, x := range []struct {
		in  *string
		out **int64
	}{{o.Upload, &p.Upload}, {o.Download, &p.Download}} {
		if x.in == nil {
			continue
		}
		v, err := units.ParseRate(*x.in)
		if err != nil {
			return p, err
		}
		*x.out = &v
	}
	if o.MaxConns != nil && *o.MaxConns < 0 {
		return p, errors.New("max_conns must be >= 0")
	}
	p.MaxConns = o.MaxConns
	return p, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
