package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Redis    RedisConfig    `yaml:"redis"`
	RabbitMQ RabbitMQConfig `yaml:"rabbitmq"` //
	Search   SearchConfig   `yaml:"search"`   // 检索的参数
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

// SearchConfig 检索的调参。全部有默认值（见 search/entity.go 的 withDefaults），
// 所以这一段在 yaml 里可以整个不写。
//
// **这里只有词法那一路的参数**：向量那一路本轮先跳过，相应地
// dim（向量维度）和 max_distance（余弦距离上限）两项目前没有任何读者，
// 就一起拿掉了 —— 留着会在 yaml 里变成"改了没反应"的假旋钮，
// 那种东西比缺参数更费时间。接向量时再加回来。
type SearchConfig struct {
	// Depth 召回的条数（融合前的候选池）。
	// 和 limit（每页展示几条）是两件事：depth 决定候选池，limit 决定切多少
	Depth int `yaml:"depth"`
	// RRFK RRF 公式里的 k（60 是 Cormack 2009 的经验值）。
	// 本轮只有词法一路时它不影响排序（单路融合恒等于该路名次），
	// 留着是为了接上向量时不用再动配置
	RRFK int `yaml:"rrf_k"`
}

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

type RedisConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type RabbitMQConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Load 读取 yaml 配置文件并解析成 Config
func Load(filename string) (Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}
