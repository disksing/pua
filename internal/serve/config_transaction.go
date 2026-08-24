package serve

// configMutation patches fields owned by one serve.json writer. The boolean
// reports whether the mutation changed the latest snapshot; version/profile
// upgrades are tracked separately by the transaction implementation.
type configMutation func(*config) (bool, error)

// transactConfig serializes every read-modify-write of serve.json with
// Workspace service lifecycle commits. Slow validation belongs before this
// seam; the mutation receives a freshly reloaded snapshot and must only patch
// fields owned by its operation.
func (s *server) transactConfig(mutate configMutation) (config, error) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	return s.transactConfigLocked(mutate)
}

// transactConfigLocked is the lifecycle-aware form used when callers already
// hold serviceMu while coordinating config, managers, and Workspace locks.
func (s *server) transactConfigLocked(mutate configMutation) (config, error) {
	cfg, needsUpgrade, err := readServeConfigFile(s.config)
	if err != nil {
		return config{}, err
	}
	changed := false
	if mutate != nil {
		changed, err = mutate(&cfg)
		if err != nil {
			return config{}, err
		}
	}
	if needsUpgrade || changed {
		if err := s.saveConfig(cfg); err != nil {
			return config{}, err
		}
	}
	return cfg, nil
}
