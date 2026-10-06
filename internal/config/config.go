package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Database    string `yaml:"database"`
	LogBodies   *bool  `yaml:"logBodies"`   // 记录请求/响应体(调试用),默认开启
	ScanWorkers *int   `yaml:"scanWorkers"` // 手动覆盖扫描并发(默认按机器性能自动判定)
}

// Load 读取配置文件;文件不存在时返回默认配置。
// 书库不在配置中管理:启动后在网页「管理」页添加。
func Load(path string) (*Config, error) {
	c := &Config{
		Host:     "",
		Port:     8080,
		Database: "data/komga-lite.db",
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Port == 0 {
		c.Port = 8080
	}
	if c.Database == "" {
		c.Database = "data/komga-lite.db"
	}
	return c, nil
}

// LogBodiesOn 返回是否记录请求/响应体;配置缺省时默认开启。
func (c *Config) LogBodiesOn() bool {
	return c.LogBodies == nil || *c.LogBodies
}
