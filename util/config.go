package ssh_ca_util

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
)

type RequesterConfig struct {
	PublicKeyPath string
	SignerUrl     string
}

type SignerdConfig struct {
	SigningKeyFingerprint string
	AuthorizedSigners     map[string]string
	AuthorizedUsers       map[string]string
	NumberSignersRequired int
}

type SignerConfig struct {
	KeyFingerprint string
	SignerUrl      string
}

func LoadConfig(configPath string, environmentConfigs interface{}) error {
	buf, err := ioutil.ReadFile(configPath)
	if err != nil {
		return err
	}

	switch configType := environmentConfigs.(type) {
	case *map[string]RequesterConfig, *map[string]SignerConfig, *map[string]SignerdConfig:
		return json.Unmarshal(buf, &environmentConfigs)
	default:
		return fmt.Errorf("oops: %T\n", configType)
	}
}