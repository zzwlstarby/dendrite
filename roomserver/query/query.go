// Copyright 2017 Vector Creations Ltd
// Copyright 2018 New Vector Ltd
// Copyright 2019-2020 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package query

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/matrix-org/dendrite/common"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/roomserver/auth"
	"github.com/matrix-org/dendrite/roomserver/state"
	"github.com/matrix-org/dendrite/roomserver/state/database"
	"github.com/matrix-org/dendrite/roomserver/types"
	"github.com/matrix-org/dendrite/roomserver/version"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
)

// RoomserverQueryAPIEventDB has a convenience API to fetch events directly by
// EventIDs.
type RoomserverQueryAPIEventDB interface {
	// Look up the Events for a list of event IDs. Does not error if event was
	// not found.
	// Returns an error if the retrieval went wrong.
	EventsFromIDs(ctx context.Context, eventIDs []string) ([]types.Event, error)
}

// RoomserverQueryAPIDatabase has the storage APIs needed to implement the query API.
type RoomserverQueryAPIDatabase interface {
	database.RoomStateDatabase
	RoomserverQueryAPIEventDB
	// Look up the numeric ID for the room.
	// Returns 0 if the room doesn't exists.
	// Returns an error if there was a problem talking to the database.
	RoomNID(ctx context.Context, roomID string) (types.RoomNID, error)
	// Look up event references for the latest events in the room and the current state snapshot.
	// Returns the latest events, the current state and the maximum depth of the latest events plus 1.
	// Returns an error if there was a problem talking to the database.
	LatestEventIDs(
		ctx context.Context, roomNID types.RoomNID,
	) ([]gomatrixserverlib.EventReference, types.StateSnapshotNID, int64, error)
	// Look up the numeric IDs for a list of events.
	// Returns an error if there was a problem talking to the database.
	EventNIDs(ctx context.Context, eventIDs []string) (map[string]types.EventNID, error)
	// Lookup the event IDs for a batch of event numeric IDs.
	// Returns an error if the retrieval went wrong.
	EventIDs(ctx context.Context, eventNIDs []types.EventNID) (map[types.EventNID]string, error)
	// Lookup the membership of a given user in a given room.
	// Returns the numeric ID of the latest membership event sent from this user
	// in this room, along a boolean set to true if the user is still in this room,
	// false if not.
	// Returns an error if there was a problem talking to the database.
	GetMembership(
		ctx context.Context, roomNID types.RoomNID, requestSenderUserID string,
	) (membershipEventNID types.EventNID, stillInRoom bool, err error)
	// Lookup the membership event numeric IDs for all user that are or have
	// been members of a given room. Only lookup events of "join" membership if
	// joinOnly is set to true.
	// Returns an error if there was a problem talking to the database.
	GetMembershipEventNIDsForRoom(
		ctx context.Context, roomNID types.RoomNID, joinOnly bool,
	) ([]types.EventNID, error)
	// Look up the active invites targeting a user in a room and return the
	// numeric state key IDs for the user IDs who sent them.
	// Returns an error if there was a problem talking to the database.
	GetInvitesForUser(
		ctx context.Context,
		roomNID types.RoomNID,
		targetUserNID types.EventStateKeyNID,
	) (senderUserNIDs []types.EventStateKeyNID, err error)
	// Look up the string event state keys for a list of numeric event state keys
	// Returns an error if there was a problem talking to the database.
	EventStateKeys(
		context.Context, []types.EventStateKeyNID,
	) (map[types.EventStateKeyNID]string, error)
	// Look up the room version from the database.
	GetRoomVersionForRoom(
		ctx context.Context, roomNID types.RoomNID,
	) (state.StateResolutionVersion, error)
}

// RoomserverQueryAPI is an implementation of api.RoomserverQueryAPI
type RoomserverQueryAPI struct {
	DB RoomserverQueryAPIDatabase
}

// QueryLatestEventsAndState implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryLatestEventsAndState(
	ctx context.Context,
	request *api.QueryLatestEventsAndStateRequest,
	response *api.QueryLatestEventsAndStateResponse,
) error {
	response.QueryLatestEventsAndStateRequest = *request
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}
	if roomNID == 0 {
		return nil
	}
	roomVersion, err := r.DB.GetRoomVersionForRoom(ctx, roomNID)
	if err != nil {
		return err
	}
	roomState, err := state.GetStateResolutionAlgorithm(roomVersion, r.DB)
	if err != nil {
		return err
	}
	response.RoomExists = true
	var currentStateSnapshotNID types.StateSnapshotNID
	response.LatestEvents, currentStateSnapshotNID, response.Depth, err =
		r.DB.LatestEventIDs(ctx, roomNID)
	if err != nil {
		return err
	}

	// Look up the currrent state for the requested tuples.
	stateEntries, err := roomState.LoadStateAtSnapshotForStringTuples(
		ctx, currentStateSnapshotNID, request.StateToFetch,
	)
	if err != nil {
		return err
	}

	stateEvents, err := r.loadStateEvents(ctx, stateEntries)
	if err != nil {
		return err
	}

	response.StateEvents = stateEvents
	return nil
}

// QueryStateAfterEvents implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryStateAfterEvents(
	ctx context.Context,
	request *api.QueryStateAfterEventsRequest,
	response *api.QueryStateAfterEventsResponse,
) error {
	response.QueryStateAfterEventsRequest = *request
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}
	if roomNID == 0 {
		return nil
	}
	response.RoomExists = true
	roomVersion, err := r.DB.GetRoomVersionForRoom(ctx, roomNID)
	if err != nil {
		return err
	}
	roomState, err := state.GetStateResolutionAlgorithm(roomVersion, r.DB)
	if err != nil {
		return err
	}
	prevStates, err := r.DB.StateAtEventIDs(ctx, request.PrevEventIDs)
	if err != nil {
		switch err.(type) {
		case types.MissingEventError:
			return nil
		default:
			return err
		}
	}
	response.PrevEventsExist = true

	// Look up the currrent state for the requested tuples.
	stateEntries, err := roomState.LoadStateAfterEventsForStringTuples(
		ctx, prevStates, request.StateToFetch,
	)
	if err != nil {
		return err
	}

	stateEvents, err := r.loadStateEvents(ctx, stateEntries)
	if err != nil {
		return err
	}

	response.StateEvents = stateEvents
	return nil
}

// QueryEventsByID implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryEventsByID(
	ctx context.Context,
	request *api.QueryEventsByIDRequest,
	response *api.QueryEventsByIDResponse,
) error {
	response.QueryEventsByIDRequest = *request

	eventNIDMap, err := r.DB.EventNIDs(ctx, request.EventIDs)
	if err != nil {
		return err
	}

	var eventNIDs []types.EventNID
	for _, nid := range eventNIDMap {
		eventNIDs = append(eventNIDs, nid)
	}

	events, err := r.loadEvents(ctx, eventNIDs)
	if err != nil {
		return err
	}

	response.Events = events
	return nil
}

func (r *RoomserverQueryAPI) loadStateEvents(
	ctx context.Context, stateEntries []types.StateEntry,
) ([]gomatrixserverlib.Event, error) {
	eventNIDs := make([]types.EventNID, len(stateEntries))
	for i := range stateEntries {
		eventNIDs[i] = stateEntries[i].EventNID
	}
	return r.loadEvents(ctx, eventNIDs)
}

func (r *RoomserverQueryAPI) loadEvents(
	ctx context.Context, eventNIDs []types.EventNID,
) ([]gomatrixserverlib.Event, error) {
	stateEvents, err := r.DB.Events(ctx, eventNIDs)
	if err != nil {
		return nil, err
	}

	result := make([]gomatrixserverlib.Event, len(stateEvents))
	for i := range stateEvents {
		result[i] = stateEvents[i].Event
	}
	return result, nil
}

// QueryMembershipForUser implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryMembershipForUser(
	ctx context.Context,
	request *api.QueryMembershipForUserRequest,
	response *api.QueryMembershipForUserResponse,
) error {
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}

	membershipEventNID, stillInRoom, err := r.DB.GetMembership(ctx, roomNID, request.UserID)
	if err != nil {
		return err
	}

	if membershipEventNID == 0 {
		response.HasBeenInRoom = false
		return nil
	}

	response.IsInRoom = stillInRoom
	eventIDMap, err := r.DB.EventIDs(ctx, []types.EventNID{membershipEventNID})
	if err != nil {
		return err
	}

	response.EventID = eventIDMap[membershipEventNID]
	return nil
}

// QueryMembershipsForRoom implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryMembershipsForRoom(
	ctx context.Context,
	request *api.QueryMembershipsForRoomRequest,
	response *api.QueryMembershipsForRoomResponse,
) error {
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}

	membershipEventNID, stillInRoom, err := r.DB.GetMembership(ctx, roomNID, request.Sender)
	if err != nil {
		return err
	}

	if membershipEventNID == 0 {
		response.HasBeenInRoom = false
		response.JoinEvents = nil
		return nil
	}

	response.HasBeenInRoom = true
	response.JoinEvents = []gomatrixserverlib.ClientEvent{}

	var events []types.Event
	if stillInRoom {
		var eventNIDs []types.EventNID
		eventNIDs, err = r.DB.GetMembershipEventNIDsForRoom(ctx, roomNID, request.JoinedOnly)
		if err != nil {
			return err
		}

		events, err = r.DB.Events(ctx, eventNIDs)
	} else {
		events, err = r.getMembershipsBeforeEventNID(ctx, membershipEventNID, request.JoinedOnly)
	}

	if err != nil {
		return err
	}

	for _, event := range events {
		clientEvent := gomatrixserverlib.ToClientEvent(event.Event, gomatrixserverlib.FormatAll)
		response.JoinEvents = append(response.JoinEvents, clientEvent)
	}

	return nil
}

// getMembershipsBeforeEventNID takes the numeric ID of an event and fetches the state
// of the event's room as it was when this event was fired, then filters the state events to
// only keep the "m.room.member" events with a "join" membership. These events are returned.
// Returns an error if there was an issue fetching the events.
func (r *RoomserverQueryAPI) getMembershipsBeforeEventNID(
	ctx context.Context, eventNID types.EventNID, joinedOnly bool,
) ([]types.Event, error) {
	// TODO: get the correct room version
	roomState, err := state.GetStateResolutionAlgorithm(state.StateResolutionAlgorithmV1, r.DB)
	if err != nil {
		return []types.Event{}, err
	}
	events := []types.Event{}
	// Lookup the event NID
	eIDs, err := r.DB.EventIDs(ctx, []types.EventNID{eventNID})
	if err != nil {
		return nil, err
	}
	eventIDs := []string{eIDs[eventNID]}

	prevState, err := r.DB.StateAtEventIDs(ctx, eventIDs)
	if err != nil {
		return nil, err
	}

	// Fetch the state as it was when this event was fired
	stateEntries, err := roomState.LoadCombinedStateAfterEvents(ctx, prevState)
	if err != nil {
		return nil, err
	}

	var eventNIDs []types.EventNID
	for _, entry := range stateEntries {
		// Filter the events to retrieve to only keep the membership events
		if entry.EventTypeNID == types.MRoomMemberNID {
			eventNIDs = append(eventNIDs, entry.EventNID)
		}
	}

	// Get all of the events in this state
	stateEvents, err := r.DB.Events(ctx, eventNIDs)
	if err != nil {
		return nil, err
	}

	if !joinedOnly {
		return stateEvents, nil
	}

	// Filter the events to only keep the "join" membership events
	for _, event := range stateEvents {
		membership, err := event.Membership()
		if err != nil {
			return nil, err
		}

		if membership == gomatrixserverlib.Join {
			events = append(events, event)
		}
	}

	return events, nil
}

// QueryInvitesForUser implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryInvitesForUser(
	ctx context.Context,
	request *api.QueryInvitesForUserRequest,
	response *api.QueryInvitesForUserResponse,
) error {
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}

	targetUserNIDs, err := r.DB.EventStateKeyNIDs(ctx, []string{request.TargetUserID})
	if err != nil {
		return err
	}
	targetUserNID := targetUserNIDs[request.TargetUserID]

	senderUserNIDs, err := r.DB.GetInvitesForUser(ctx, roomNID, targetUserNID)
	if err != nil {
		return err
	}

	senderUserIDs, err := r.DB.EventStateKeys(ctx, senderUserNIDs)
	if err != nil {
		return err
	}

	for _, senderUserID := range senderUserIDs {
		response.InviteSenderUserIDs = append(response.InviteSenderUserIDs, senderUserID)
	}

	return nil
}

// QueryServerAllowedToSeeEvent implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryServerAllowedToSeeEvent(
	ctx context.Context,
	request *api.QueryServerAllowedToSeeEventRequest,
	response *api.QueryServerAllowedToSeeEventResponse,
) (err error) {
	response.AllowedToSeeEvent, err = r.checkServerAllowedToSeeEvent(
		ctx, request.EventID, request.ServerName,
	)
	return
}

func (r *RoomserverQueryAPI) checkServerAllowedToSeeEvent(
	ctx context.Context, eventID string, serverName gomatrixserverlib.ServerName,
) (bool, error) {
	// TODO: get the correct room version
	roomState, err := state.GetStateResolutionAlgorithm(state.StateResolutionAlgorithmV1, r.DB)
	if err != nil {
		return false, err
	}

	stateEntries, err := roomState.LoadStateAtEvent(ctx, eventID)
	if err != nil {
		return false, err
	}

	// TODO: We probably want to make it so that we don't have to pull
	// out all the state if possible.
	stateAtEvent, err := r.loadStateEvents(ctx, stateEntries)
	if err != nil {
		return false, err
	}

	return auth.IsServerAllowed(serverName, stateAtEvent), nil
}

// QueryMissingEvents implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryMissingEvents(
	ctx context.Context,
	request *api.QueryMissingEventsRequest,
	response *api.QueryMissingEventsResponse,
) error {
	var front []string
	eventsToFilter := make(map[string]bool, len(request.LatestEvents))
	visited := make(map[string]bool, request.Limit) // request.Limit acts as a hint to size.
	for _, id := range request.EarliestEvents {
		visited[id] = true
	}

	for _, id := range request.LatestEvents {
		if !visited[id] {
			front = append(front, id)
			eventsToFilter[id] = true
		}
	}

	resultNIDs, err := r.scanEventTree(ctx, front, visited, request.Limit, request.ServerName)
	if err != nil {
		return err
	}

	loadedEvents, err := r.loadEvents(ctx, resultNIDs)
	if err != nil {
		return err
	}

	response.Events = make([]gomatrixserverlib.Event, 0, len(loadedEvents)-len(eventsToFilter))
	for _, event := range loadedEvents {
		if !eventsToFilter[event.EventID()] {
			response.Events = append(response.Events, event)
		}
	}

	return err
}

// QueryBackfill implements api.RoomServerQueryAPI
func (r *RoomserverQueryAPI) QueryBackfill(
	ctx context.Context,
	request *api.QueryBackfillRequest,
	response *api.QueryBackfillResponse,
) error {
	var err error
	var front []string

	// The limit defines the maximum number of events to retrieve, so it also
	// defines the highest number of elements in the map below.
	visited := make(map[string]bool, request.Limit)

	// The provided event IDs have already been seen by the request's emitter,
	// and will be retrieved anyway, so there's no need to care about them if
	// they appear in our exploration of the event tree.
	for _, id := range request.EarliestEventsIDs {
		visited[id] = true
	}

	front = request.EarliestEventsIDs

	// Scan the event tree for events to send back.
	resultNIDs, err := r.scanEventTree(ctx, front, visited, request.Limit, request.ServerName)
	if err != nil {
		return err
	}

	// Retrieve events from the list that was filled previously.
	response.Events, err = r.loadEvents(ctx, resultNIDs)
	return err
}

func (r *RoomserverQueryAPI) scanEventTree(
	ctx context.Context, front []string, visited map[string]bool, limit int,
	serverName gomatrixserverlib.ServerName,
) (resultNIDs []types.EventNID, err error) {
	var allowed bool
	var events []types.Event
	var next []string
	var pre string

	resultNIDs = make([]types.EventNID, 0, limit)

	// Loop through the event IDs to retrieve the requested events and go
	// through the whole tree (up to the provided limit) using the events'
	// "prev_event" key.
BFSLoop:
	for len(front) > 0 {
		// Prevent unnecessary allocations: reset the slice only when not empty.
		if len(next) > 0 {
			next = make([]string, 0)
		}
		// Retrieve the events to process from the database.
		events, err = r.DB.EventsFromIDs(ctx, front)
		if err != nil {
			return
		}

		for _, ev := range events {
			// Break out of the loop if the provided limit is reached.
			if len(resultNIDs) == limit {
				break BFSLoop
			}
			// Update the list of events to retrieve.
			resultNIDs = append(resultNIDs, ev.EventNID)
			// Loop through the event's parents.
			for _, pre = range ev.PrevEventIDs() {
				// Only add an event to the list of next events to process if it
				// hasn't been seen before.
				if !visited[pre] {
					visited[pre] = true
					allowed, err = r.checkServerAllowedToSeeEvent(ctx, pre, serverName)
					if err != nil {
						return
					}

					// If the event hasn't been seen before and the HS
					// requesting to retrieve it is allowed to do so, add it to
					// the list of events to retrieve.
					if allowed {
						next = append(next, pre)
					}
				}
			}
		}
		// Repeat the same process with the parent events we just processed.
		front = next
	}

	return
}

// QueryStateAndAuthChain implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryStateAndAuthChain(
	ctx context.Context,
	request *api.QueryStateAndAuthChainRequest,
	response *api.QueryStateAndAuthChainResponse,
) error {
	// TODO: get the correct room version
	roomState, err := state.GetStateResolutionAlgorithm(state.StateResolutionAlgorithmV1, r.DB)
	if err != nil {
		return err
	}

	response.QueryStateAndAuthChainRequest = *request
	roomNID, err := r.DB.RoomNID(ctx, request.RoomID)
	if err != nil {
		return err
	}
	if roomNID == 0 {
		return nil
	}
	response.RoomExists = true

	prevStates, err := r.DB.StateAtEventIDs(ctx, request.PrevEventIDs)
	if err != nil {
		switch err.(type) {
		case types.MissingEventError:
			return nil
		default:
			return err
		}
	}
	response.PrevEventsExist = true

	// Look up the currrent state for the requested tuples.
	stateEntries, err := roomState.LoadCombinedStateAfterEvents(
		ctx, prevStates,
	)
	if err != nil {
		return err
	}

	stateEvents, err := r.loadStateEvents(ctx, stateEntries)
	if err != nil {
		return err
	}

	response.StateEvents = stateEvents
	response.AuthChainEvents, err = getAuthChain(ctx, r.DB, request.AuthEventIDs)
	return err
}

// getAuthChain fetches the auth chain for the given auth events. An auth chain
// is the list of all events that are referenced in the auth_events section, and
// all their auth_events, recursively. The returned set of events contain the
// given events. Will *not* error if we don't have all auth events.
func getAuthChain(
	ctx context.Context, dB RoomserverQueryAPIEventDB, authEventIDs []string,
) ([]gomatrixserverlib.Event, error) {
	// List of event IDs to fetch. On each pass, these events will be requested
	// from the database and the `eventsToFetch` will be updated with any new
	// events that we have learned about and need to find. When `eventsToFetch`
	// is eventually empty, we should have reached the end of the chain.
	eventsToFetch := authEventIDs
	authEventsMap := make(map[string]gomatrixserverlib.Event)

	for len(eventsToFetch) > 0 {
		// Try to retrieve the events from the database.
		events, err := dB.EventsFromIDs(ctx, eventsToFetch)
		if err != nil {
			return nil, err
		}

		// We've now fetched these events so clear out `eventsToFetch`. Soon we may
		// add newly discovered events to this for the next pass.
		eventsToFetch = eventsToFetch[:0]

		for _, event := range events {
			// Store the event in the event map - this prevents us from requesting it
			// from the database again.
			authEventsMap[event.EventID()] = event.Event

			// Extract all of the auth events from the newly obtained event. If we
			// don't already have a record of the event, record it in the list of
			// events we want to request for the next pass.
			for _, authEvent := range event.AuthEvents() {
				if _, ok := authEventsMap[authEvent.EventID]; !ok {
					eventsToFetch = append(eventsToFetch, authEvent.EventID)
				}
			}
		}
	}

	// We've now retrieved all of the events we can. Flatten them down into an
	// array and return them.
	var authEvents []gomatrixserverlib.Event
	for _, event := range authEventsMap {
		authEvents = append(authEvents, event)
	}

	return authEvents, nil
}

// QueryServersInRoomAtEvent implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryServersInRoomAtEvent(
	ctx context.Context,
	request *api.QueryServersInRoomAtEventRequest,
	response *api.QueryServersInRoomAtEventResponse,
) error {
	// getMembershipsBeforeEventNID requires a NID, so retrieving the NID for
	// the event is necessary.
	NIDs, err := r.DB.EventNIDs(ctx, []string{request.EventID})
	if err != nil {
		return err
	}

	// Retrieve all "m.room.member" state events of "join" membership, which
	// contains the list of users in the room before the event, therefore all
	// the servers in it at that moment.
	events, err := r.getMembershipsBeforeEventNID(ctx, NIDs[request.EventID], true)
	if err != nil {
		return err
	}

	// Store the server names in a temporary map to avoid duplicates.
	servers := make(map[gomatrixserverlib.ServerName]bool)
	for _, event := range events {
		servers[event.Origin()] = true
	}

	// Populate the response.
	for server := range servers {
		response.Servers = append(response.Servers, server)
	}

	return nil
}

// QueryRoomVersionCapabilities implements api.RoomserverQueryAPI
func (r *RoomserverQueryAPI) QueryRoomVersionCapabilities(
	ctx context.Context,
	request *api.QueryRoomVersionCapabilitiesRequest,
	response *api.QueryRoomVersionCapabilitiesResponse,
) error {
	response.DefaultRoomVersion = strconv.Itoa(int(version.GetDefaultRoomVersion()))
	response.AvailableRoomVersions = make(map[string]string)
	for v, desc := range version.GetSupportedRoomVersions() {
		sv := strconv.Itoa(int(v))
		if desc.Stable {
			response.AvailableRoomVersions[sv] = "stable"
		} else {
			response.AvailableRoomVersions[sv] = "unstable"
		}
	}
	return nil
}

// SetupHTTP adds the RoomserverQueryAPI handlers to the http.ServeMux.
// nolint: gocyclo
func (r *RoomserverQueryAPI) SetupHTTP(servMux *http.ServeMux) {
	servMux.Handle(
		api.RoomserverQueryLatestEventsAndStatePath,
		common.MakeInternalAPI("queryLatestEventsAndState", func(req *http.Request) util.JSONResponse {
			var request api.QueryLatestEventsAndStateRequest
			var response api.QueryLatestEventsAndStateResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryLatestEventsAndState(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryStateAfterEventsPath,
		common.MakeInternalAPI("queryStateAfterEvents", func(req *http.Request) util.JSONResponse {
			var request api.QueryStateAfterEventsRequest
			var response api.QueryStateAfterEventsResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryStateAfterEvents(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryEventsByIDPath,
		common.MakeInternalAPI("queryEventsByID", func(req *http.Request) util.JSONResponse {
			var request api.QueryEventsByIDRequest
			var response api.QueryEventsByIDResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryEventsByID(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryMembershipForUserPath,
		common.MakeInternalAPI("QueryMembershipForUser", func(req *http.Request) util.JSONResponse {
			var request api.QueryMembershipForUserRequest
			var response api.QueryMembershipForUserResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryMembershipForUser(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryMembershipsForRoomPath,
		common.MakeInternalAPI("queryMembershipsForRoom", func(req *http.Request) util.JSONResponse {
			var request api.QueryMembershipsForRoomRequest
			var response api.QueryMembershipsForRoomResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryMembershipsForRoom(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryInvitesForUserPath,
		common.MakeInternalAPI("queryInvitesForUser", func(req *http.Request) util.JSONResponse {
			var request api.QueryInvitesForUserRequest
			var response api.QueryInvitesForUserResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryInvitesForUser(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryServerAllowedToSeeEventPath,
		common.MakeInternalAPI("queryServerAllowedToSeeEvent", func(req *http.Request) util.JSONResponse {
			var request api.QueryServerAllowedToSeeEventRequest
			var response api.QueryServerAllowedToSeeEventResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryServerAllowedToSeeEvent(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryMissingEventsPath,
		common.MakeInternalAPI("queryMissingEvents", func(req *http.Request) util.JSONResponse {
			var request api.QueryMissingEventsRequest
			var response api.QueryMissingEventsResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryMissingEvents(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryStateAndAuthChainPath,
		common.MakeInternalAPI("queryStateAndAuthChain", func(req *http.Request) util.JSONResponse {
			var request api.QueryStateAndAuthChainRequest
			var response api.QueryStateAndAuthChainResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryStateAndAuthChain(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryBackfillPath,
		common.MakeInternalAPI("QueryBackfill", func(req *http.Request) util.JSONResponse {
			var request api.QueryBackfillRequest
			var response api.QueryBackfillResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryBackfill(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryServersInRoomAtEventPath,
		common.MakeInternalAPI("QueryServersInRoomAtEvent", func(req *http.Request) util.JSONResponse {
			var request api.QueryServersInRoomAtEventRequest
			var response api.QueryServersInRoomAtEventResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryServersInRoomAtEvent(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
	servMux.Handle(
		api.RoomserverQueryRoomVersionCapabilitiesPath,
		common.MakeInternalAPI("QueryRoomVersionCapabilities", func(req *http.Request) util.JSONResponse {
			var request api.QueryRoomVersionCapabilitiesRequest
			var response api.QueryRoomVersionCapabilitiesResponse
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				return util.ErrorResponse(err)
			}
			if err := r.QueryRoomVersionCapabilities(req.Context(), &request, &response); err != nil {
				return util.ErrorResponse(err)
			}
			return util.JSONResponse{Code: http.StatusOK, JSON: &response}
		}),
	)
}
