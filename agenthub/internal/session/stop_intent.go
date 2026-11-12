package session

import "encoding/json"

// StopWasRequested recovers an explicit stop intent without publishing a
// stopped boundary before provider cleanup has completed.
func (s *Store) StopWasRequested(id string) (bool, error) {
	state, err := s.state(id)
	if err != nil {
		return false, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := s.ensureEventsLocked(state, id); err != nil {
		return false, err
	}
	for index := len(state.events) - 1; index >= 0; index-- {
		event := state.events[index]
		if event.Type != "session.state" {
			continue
		}
		var data StateEventData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if data.State == StateStopping && data.Reason == "" {
			continue
		}
		return (data.State == StateStopping || data.State == StateStopped) && data.Reason == StopReasonRequested, nil
	}
	return false, nil
}
