package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mirmik/ariadne/internal/execspec"
	"github.com/mirmik/ariadne/internal/wire"
)

func (server *Server) handleJobs(response http.ResponseWriter, request *http.Request, session *nodeSession) {
	if !wire.HasCapability(session.nodeInfo().Capabilities, wire.CapabilityBackgroundJobs) {
		writeAPIError(response, http.StatusConflict, "connector does not support background jobs; update ariadne-connector")
		return
	}
	if request.Method == http.MethodGet {
		result, err := session.job(request.Context(), wire.JobRequest{Action: wire.JobActionList})
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(response, http.StatusOK, result)
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	var execRequest wire.ExecRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&execRequest); err != nil {
		writeAPIError(response, http.StatusBadRequest, "invalid job request: "+err.Error())
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeAPIError(response, http.StatusBadRequest, "invalid job request: unexpected data after JSON value")
		return
	}
	displayCommand := execRequest.Command
	prepared, usedShell, err := execspec.Prepare(execRequest, session.nodeInfo().Platform)
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, err.Error())
		return
	}
	if prepared.TimeoutMillis < 0 || prepared.TimeoutMillis > server.config.MaxJobTimeout.Milliseconds() {
		writeAPIError(response, http.StatusBadRequest, fmt.Sprintf("job timeout must be non-negative and not exceed %s", server.config.MaxJobTimeout))
		return
	}
	result, err := session.job(request.Context(), wire.JobRequest{Action: wire.JobActionStart, Exec: &prepared, Command: displayCommand, Shell: usedShell})
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(response, http.StatusCreated, result)
}

func (server *Server) handleJob(response http.ResponseWriter, request *http.Request, session *nodeSession, remainder string) {
	if !wire.HasCapability(session.nodeInfo().Capabilities, wire.CapabilityBackgroundJobs) {
		writeAPIError(response, http.StatusConflict, "connector does not support background jobs; update ariadne-connector")
		return
	}
	parts := strings.Split(remainder, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		writeAPIError(response, http.StatusNotFound, "unknown job endpoint")
		return
	}
	jobID, err := url.PathUnescape(parts[0])
	if err != nil || jobID == "" {
		writeAPIError(response, http.StatusBadRequest, "invalid job ID")
		return
	}
	action := wire.JobActionStatus
	jobRequest := wire.JobRequest{JobID: jobID}
	switch {
	case len(parts) == 1 && request.Method == http.MethodGet:
		action = wire.JobActionStatus
	case len(parts) == 1 && request.Method == http.MethodDelete:
		action = wire.JobActionRemove
	case len(parts) == 2 && parts[1] == "cancel" && request.Method == http.MethodPost:
		action = wire.JobActionCancel
	case len(parts) == 2 && parts[1] == "output" && request.Method == http.MethodGet:
		action = wire.JobActionRead
		stdoutOffset, parseErr := parseNonNegativeInt64(request.URL.Query().Get("stdout_offset"), "stdout_offset")
		if parseErr != nil {
			writeAPIError(response, http.StatusBadRequest, parseErr.Error())
			return
		}
		stderrOffset, parseErr := parseNonNegativeInt64(request.URL.Query().Get("stderr_offset"), "stderr_offset")
		if parseErr != nil {
			writeAPIError(response, http.StatusBadRequest, parseErr.Error())
			return
		}
		limit := 0
		if rawLimit := request.URL.Query().Get("limit"); rawLimit != "" {
			parsed, err := strconv.Atoi(rawLimit)
			if err != nil {
				writeAPIError(response, http.StatusBadRequest, "limit must be an integer")
				return
			}
			limit = parsed
		}
		jobRequest.StdoutOffset = stdoutOffset
		jobRequest.StderrOffset = stderrOffset
		jobRequest.Limit = limit
	default:
		writeAPIError(response, http.StatusNotFound, "unknown job endpoint")
		return
	}
	jobRequest.Action = action
	result, err := session.job(request.Context(), jobRequest)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func parseNonNegativeInt64(raw, name string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}
