package serve

// These low-level helpers initialize isolated test fixtures before concurrent
// server work begins. Production serve.json mutations use transactConfig.
func writeAgentHubConfigFile(path string, cfg agentHubServeConfig) error {
	return (&server{config: path}).saveConfig(serveConfigFromAgentHubConfig(cfg))
}

func readAgentHubConfigFile(path string) (agentHubServeConfig, error) {
	cfg, _, err := readServeConfigFile(path)
	return agentHubConfigFromServeConfig(cfg), err
}
