package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// Handler is how processes that are not attached to the session read its
// output, drive it, and inspect it (2.6, 2.7, 3.2). Both the developer's
// gateway and Hermes Agent enter through here (3.1).
func (s *Supervisor) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /output", func(w http.ResponseWriter, r *http.Request) {
		since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if err != nil || since < 0 {
			since = 0
		}
		chunks, err := s.ReadOutput(since)
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if chunks == nil {
			chunks = []OutputChunk{}
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"chunks": chunks})
	})

	mux.HandleFunc("POST /input", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONResponse(w, http.StatusBadRequest, &SendError{Kind: SendInputRejected, Detail: err.Error()})
			return
		}
		err := s.SendInput(body.Text)
		if err == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var sendErr *SendError
		if !errors.As(err, &sendErr) {
			writeJSONResponse(w, http.StatusInternalServerError, &SendError{Kind: SendInputRejected, Detail: err.Error()})
			return
		}
		status := http.StatusConflict
		if sendErr.Kind == SendInputRejected {
			status = http.StatusInternalServerError
		}
		writeJSONResponse(w, status, sendErr)
	})

	mux.HandleFunc("GET /clients", func(w http.ResponseWriter, r *http.Request) {
		clients, err := s.ListClients()
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, clients)
	})

	mux.HandleFunc("GET /browser-verification", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, http.StatusOK, s.InteractiveBrowserVerification())
	})

	mux.HandleFunc("GET /ssh-sessions", func(w http.ResponseWriter, r *http.Request) {
		count, err := s.SSHSessionCount()
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]int{"count": count})
	})

	mux.HandleFunc("GET /changed-files", func(w http.ResponseWriter, r *http.Request) {
		files, err := ChangedFiles(s.WorkingDir)
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if files == nil {
			files = []string{}
		}
		writeJSONResponse(w, http.StatusOK, map[string][]string{"files": files})
	})

	mux.HandleFunc("GET /turn", func(w http.ResponseWriter, r *http.Request) {
		state, err := s.TurnState()
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, state)
	})

	return mux
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
