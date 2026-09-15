package config

import (
	"os"
	"runtime"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Redis     RedisConfig     `yaml:"redis"`
	RabbitMQ  RabbitMQConfig  `yaml:"rabbitmq"` //
	Search    SearchConfig    `yaml:"search"`   // 检索的参数
	Storage   StorageConfig   `yaml:"storage"`
	Transcode TranscodeConfig `yaml:"transcode"`
}

// StorageConfig 上传文件的落盘位置。**整段可以不在 yaml 里写**（有默认值）。
//
// 为什么它值得一个配置项而不是继续硬编码：这个路径现在**有两个进程要读** ——
// API 进程（r.Static 把它挂成 /static）和 worker 进程（转码要读原文件、
// 写 HLS 产物）。两边各写一个字面量的话，改一边忘一边的表现是
// "转码说找不到源文件"，而两边看起来都"没改过什么"。
//
// ⚠ **写文件的地方不要直接用 UploadRoot 拼路径**，用 paths.go 里的
// VideosDir / AvatarsDir / DiskPath。原因写在 paths.go 顶部：
// 这个字符串曾经手抄在五个文件里，门禁一读配置就出现了第六个来源，
// 而两边不一致的后果是**静默的**（门禁全部走"探测失败 → 直传"）。
type StorageConfig struct {
	// UploadRoot 上传根目录，对应 URL 前缀 /static。
	// 即 `/static/videos/1/xxx.mp4` 在磁盘上是 `<UploadRoot>/videos/1/xxx.mp4`。
	//
	// 相对路径是相对**进程的工作目录**解释的（和 configs/config.yaml 一样，
	// 都是 CWD 相对）。所以 API 和 worker 必须从同一个目录启动。
	UploadRoot string `yaml:"upload_root"`
}

// TranscodeConfig 转码相关的路径。
//
// 只放了**已经有读者**的项。ffprobe 的路径没有加 —— 代码里不用它
// （ffprobe 只是验收时手工核对三档参数用的），加了就是一个假旋钮。
// 带宽预算同理，等过载降级真正读它时再加。
type TranscodeConfig struct {
	// FFmpegPath ffmpeg 可执行文件的路径。
	//
	// 默认指向项目自带的 `./.run/bin/`，**不动系统 PATH**：
	// 全局安装会污染开发机环境，而且版本随人不同 ——
	// "在我机器上转码参数正常"这类问题不该出现在一个复刻项目里。
	FFmpegPath string `yaml:"ffmpeg_path"`
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

// 默认值。放在这里而不是各个使用处，是因为这些路径要被**两个进程**读到
// （API 和 worker），默认值散在两边就迟早会不一致。
const (
	// DefaultUploadRoot 上传根目录的默认值。
	//
	// **导出**是因为 StorageConfig.Root() 要用它兜底 —— 见那个函数的说明：
	// 零值 StorageConfig 必须能自愈，否则 filepath.Join("", "videos") 会把
	// 文件落到当前工作目录下，不报错、只是找不到。
	DefaultUploadRoot = "./.run/uploads"

	defaultBinDir = "./.run/bin"

	// DefaultServerPort HTTP 服务的默认端口。
	//
	// **导出**是为了让"默认端口是几"只有这一个来源：nginx 的 upstream、
	// 容器 healthcheck、部署文档里都要写这个数，各写各的就迟早会不一致。
	// （nginx 那一侧没法 import Go 常量，但至少仓库里只有一处定义，
	// 而 deploy/nginx.conf 里会写明它对应的是哪个常量。）
	DefaultServerPort = 8080
)

// exeSuffix 按平台补可执行文件后缀。
//
// 不直接写死 ".exe"：这个项目的开发机确实是 Windows（路径、TLS、zerocopy
// 那几条记录都是 Windows 特有的），但配置默认值写死平台后缀，
// 会让"在 Linux 上跑一遍"变成一件需要改代码的事 —— 而那正是
// 我们最想确认的"代码本身没有平台假设"。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// withDefaults 把空字段填上默认值。
//
// **在 Load 里统一调用**，而不是让各个使用处自己判空 —— 后者会让
// "没写这段配置"和"写了空字符串"变成两种不同的行为，而它们本该一样。
//
// 注意默认值**只在为空时生效**：配置里显式写了的（哪怕写得很怪）
// 一律尊重。这是"配置优先于约定"。
func (c *Config) withDefaults() {
	// server.port 漏写 / 写成 0 时**必须**给个默认值，否则：
	// cmd/main.go 拼出来的是 `fmt.Sprintf(":%d", 0)` = `":0"`，
	// 而 `":0"` 在 net.Listen 里的含义是"**让内核随便挑一个空闲端口**"。
	// 于是日志照常打印 `server listening on :0`，进程也起来了、也不报错 ——
	// 只是 nginx 反代 8080 得到 connection refused，而两边看不出关联。
	//
	// 这是"配置缺失不报错"的典型，也是上云时最容易踩的一条：
	// 本地 config.yaml 里一直写着 8080（所以从没暴露过），
	// 而生产那份 config.prod.yaml 是新写的，漏一行的概率不低。
	if c.Server.Port == 0 {
		c.Server.Port = DefaultServerPort
	}
	if c.Storage.UploadRoot == "" {
		c.Storage.UploadRoot = DefaultUploadRoot
	}
	if c.Transcode.FFmpegPath == "" {
		c.Transcode.FFmpegPath = defaultBinDir + "/ffmpeg" + exeSuffix()
	}
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

	cfg.withDefaults()
	return cfg, nil
}
