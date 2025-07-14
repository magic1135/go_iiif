package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"sort"
	"crypto/sha256"
    "encoding/hex"
    "math"
    "regexp"
    "net/url"
    "gopkg.in/yaml.v3"

    "github.com/go-redis/redis/v8"
	"github.com/davidbyttow/govips/v2/vips"
	"github.com/gin-gonic/gin"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// IIIF 配置
type Config struct {
	ImageDir      string     `yaml:"imageDir"`
	CacheDir      string     `yaml:"cacheDir"`
	Host          string     `yaml:"host"`
	Port          int        `yaml:"port"`
	MaxPixels     int        `yaml:"maxPixels"`
	Concurrency   int        `yaml:"concurrency"`
	EnableHTTPS   bool       `yaml:"enableHTTPS"`
	CertFile      string     `yaml:"certFile"`
	KeyFile       string     `yaml:"keyFile"`
	MinIO         MinIOConfig `yaml:"minio"`
	CacheMaxSize  int64      `yaml:"cacheMaxSize"` // 缓存最大大小(字节)
	CORS          CORSConfig `yaml:"cors"`
	ReadMinIO     bool       `yaml:"readMinIO"`
	Version     string     `yaml:"version"`
	Redis RedisConfig  `yaml:"redis"`
}
// CORS 配置
type CORSConfig struct {
    AllowOrigins     []string `yaml:"allowOrigins"`
    AllowMethods     []string `yaml:"allowMethods"`
    AllowHeaders     []string `yaml:"allowHeaders"`
    AllowCredentials bool     `yaml:"allowCredentials"`
    MaxAge           int      `yaml:"maxAge"`
}

type RedisConfig struct {
    Host     string `yaml:"host"`
    Port     int    `yaml:"port"`
    Password string `yaml:"password"`
    DB       int    `yaml:"db"`
    UseTLS   bool   `yaml:"useTLS"`
}

//缓存管理器
type CacheManager struct {
    cacheDir string
//     maxSize  int64 // 最大缓存大小(字节)
//     currentSize int64 // 当前缓存大小
    mu       sync.Mutex
    redisTTL time.Duration // Redis缓存过期时间
}

// IIIF 错误响应结构
type IIIFError struct {
    Context string `json:"@context"`
    Type    string `json:"type"`
    Error   struct {
        Code    string `json:"code"`
        Message string `json:"message"`
    } `json:"error"`
}

type MinIOConfig struct {
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"accessKey"`
	SecretKey string `yaml:"secretKey"`
	Bucket    string `yaml:"bucket"`
	UseSSL    bool   `yaml:"useSSL"`
}

// IIIF 请求参数 (v3.0)
type IIIFRequest struct {
	Identifier string
	Region     string
	Size       string
	Rotation   string
	Quality    string
	Format     string
}

// IIIF 信息响应 (v3.0)
type IIIFInfo struct {
	Context        string   `json:"@context"`
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Protocol       string   `json:"protocol"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	Sizes          []Size   `json:"sizes"`
	Tiles          []Tile   `json:"tiles"`
	Profile        []string `json:"profile"`
	ExtraFormats   []string `json:"extraFormats,omitempty"`
	ExtraQualities []string `json:"extraQualities,omitempty"`
	ExtraFeatures  []string `json:"extraFeatures,omitempty"`
}

type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type Tile struct {
	Width        int   `json:"width"`
	ScaleFactors []int `json:"scaleFactors"`
}

// 服务器状态信息
type ServerStatus struct {
	StartTime     time.Time      `json:"startTime"`
	Uptime        string         `json:"uptime"`
	GoVersion     string         `json:"goVersion"`
	NumCPU        int            `json:"numCPU"`
	NumGoroutine  int            `json:"numGoroutine"`
	MemoryStats   runtime.MemStats `json:"memoryStats"`
	CacheSize     int            `json:"cacheSize"`
	ImageCount    int            `json:"imageCount"`
}

var (
	config      Config
	vipsInit    sync.Once
	cache       sync.Map
	startTime   = time.Now()
	minioClient *minio.Client
	redisClient *redis.Client
)

func init() {
    // 加载配置文件（必须存在）
    if err := loadConfig(); err != nil {
        log.Fatalf("加载配置文件失败: %v", err)
    }

    // 确保必要的目录存在
    if err := ensureDirectories(); err != nil {
        log.Fatalf("创建必要目录失败: %v", err)
    }
    // 如果不开启minio就不用初始化redis和minio
    if config.ReadMinIO{
        // 初始化缓存管理器
        log.Println("初始化函数开始执行")
        initCacheManager()
        log.Println("缓存管理器初始化完成")
        startCacheCleaner()
        // 初始化MinIO客户端
        if err := initMinIO(); err != nil {
            log.Fatalf("初始化MinIO客户端失败: %v", err)
        }

        // 初始化Redis客户端
        if err := initRedis(); err != nil {
            log.Fatalf("初始化Redis客户端失败: %v", err)
        }
    }

}

func initRedis() error {
    redisClient = redis.NewClient(&redis.Options{
        Addr:     fmt.Sprintf("%s:%d", config.Redis.Host, config.Redis.Port),
        Password: config.Redis.Password,
        DB:       config.Redis.DB,
    })

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    _, err := redisClient.Ping(ctx).Result()
    if err != nil {
        return fmt.Errorf("无法连接到Redis: %v", err)
    }

    log.Println("✅ Redis连接成功")
    return nil
}

// 生成SHA256哈希作为缓存键
func generateCacheKey(identifier string) string {
    hash := sha256.Sum256([]byte(identifier))
    return hex.EncodeToString(hash[:])
}

func startCacheCleaner() {
    // 创建一个24小时周期的定时器
    ticker := time.NewTicker(24 * time.Hour)
    go func() {
        for range ticker.C {
            log.Println("开始执行每日缓存清理...")
            if err := cacheManager.cleanupCache(); err != nil {
                log.Printf("缓存清理失败: %v", err)

                // 失败后5分钟重试一次
                time.Sleep(5 * time.Minute)
                if err := cacheManager.cleanupCache(); err != nil {
                    log.Printf("缓存清理重试失败: %v", err)
                }
            }
        }
    }()
    log.Println("已启动每日缓存清理任务")
}

// 统一的错误响应函数
func sendIIIFError(c *gin.Context, statusCode int, errorCode, message string) {
    errResponse := IIIFError{
        Context: "http://iiif.io/api/image/3/context.json",
        Type:    "error",
    }
    errResponse.Error.Code = errorCode
    errResponse.Error.Message = message

    c.JSON(statusCode, errResponse)
}


func loadConfig() error {
    configFile := "config.yaml"

    // 检查配置文件是否存在
    if _, err := os.Stat(configFile); os.IsNotExist(err) {
        return fmt.Errorf("配置文件 %s 不存在", configFile)
    }

    // 读取配置文件
    file, err := os.Open(configFile)
    if err != nil {
        return fmt.Errorf("打开配置文件失败: %v", err)
    }
    defer file.Close()

    // yaml 解码器
    if err := yaml.NewDecoder(file).Decode(&config); err != nil {
        return fmt.Errorf("解析配置文件失败: %v", err)
    }

    return nil
}

func ensureDirectories() error {
	dirs := []string{config.ImageDir, config.CacheDir}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %v", dir, err)
		}
	}
	return nil
}

var cacheManager *CacheManager

// 初始化缓存管理器
func initCacheManager() {
    cacheManager = &CacheManager{
        cacheDir: config.CacheDir,
        redisTTL: 24 * time.Hour, // 设置Redis缓存过期时间为1小时
    }

//     if err := os.MkdirAll(cacheManager.cacheDir, 0755); err != nil {
//         log.Fatalf("创建缓存目录失败: %v", err)
//     }
}


// 获取缓存文件路径
func (cm *CacheManager) getCachePath(identifier string) string {
    // 使用identifier作为文件名，确保唯一性
    cacheKey := generateCacheKey(identifier)
    return filepath.Join(cm.cacheDir, cacheKey)
}

// 检查缓存是否存在
func (cm *CacheManager) isCached(identifier string) (bool, string) {
    ctx := context.Background()
    cacheKey := generateCacheKey(identifier)
    cachePath := cm.getCachePath(cacheKey)

    // 检查本地是否有键文件
    if _, err := os.Stat(cachePath); err != nil {
        return false, ""
    }

    // 检查Redis是否有数据
    exists, err := redisClient.Exists(ctx, cacheKey).Result()
    if err != nil {
        log.Printf("检查Redis缓存失败: %v", err)
        return false, ""
    }

    if exists == 1 {
        log.Printf("✅ 缓存命中: %s", cachePath)
        return true, cachePath
    }

    // 如果Redis没有数据，删除本地键文件（避免脏数据）
    os.Remove(cachePath)
    log.Printf("❌ 缓存未命中: %s", identifier)
    return false, ""
}

// 添加文件到缓存
func (cm *CacheManager) addToCache(identifier string, filePath string) (string, error) {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    ctx := context.Background()
    cacheKey := generateCacheKey(identifier)
    cachePath := cm.getCachePath(cacheKey)

    // 读取文件内容
    fileContent, err := os.ReadFile(filePath)
    if err != nil {
        return "", fmt.Errorf("读取文件内容失败: %v", err)
    }

    // 存入Redis（实际数据）
    if err := redisClient.Set(ctx, cacheKey, fileContent, cm.redisTTL).Err(); err != nil {
        return "", fmt.Errorf("Redis存储失败: %v", err)
    }

    // 在本地cache目录下创建一个空文件（仅作为键标记）
    if err := os.WriteFile(cachePath, []byte(""), 0644); err != nil {
        // 如果本地存储失败，回滚Redis操作
        redisClient.Del(ctx, cacheKey)
        return "", fmt.Errorf("创建缓存键文件失败: %v", err)
    }

    log.Printf("✅ 缓存成功: Key=%s, Size=%d bytes", cacheKey, len(fileContent))
    return cachePath, nil
}


func (cm *CacheManager) getFromRedis(cacheKey string) ([]byte, error) {
    ctx := context.Background()
    data, err := redisClient.Get(ctx, cacheKey).Bytes()
    if err != nil {
        return nil, fmt.Errorf("从Redis获取数据失败: %v", err)
    }
    return data, nil
}

// 清理缓存以释放指定大小的空间
func (cm *CacheManager) cleanupCache() error {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    // 清理本地键文件
    entries, err := os.ReadDir(cm.cacheDir)
    if err != nil {
        return fmt.Errorf("读取缓存目录失败: %v", err)
    }

    // 按修改时间排序（旧文件优先删除）
    sort.Slice(entries, func(i, j int) bool {
        info1, _ := entries[i].Info()
        info2, _ := entries[j].Info()
        return info1.ModTime().Before(info2.ModTime())
    })

    // 删除前N个旧文件（最多清理100个）
    maxCleanup := 100
    deleted := 0

    for _, entry := range entries {
        if deleted >= maxCleanup {
            break
        }

        filePath := filepath.Join(cm.cacheDir, entry.Name())
        if err := os.Remove(filePath); err != nil {
            log.Printf("警告: 无法删除键文件 %s: %v", filePath, err)
            continue
        }
        deleted++
    }

    log.Printf("清理完成: 删除 %d 个本地键文件", deleted)
    return nil
}

func initMinIO() error {
	var err error
	minioClient, err = minio.New(config.MinIO.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.MinIO.AccessKey, config.MinIO.SecretKey, ""),
		Secure: false,
	})
	if err != nil {
		return fmt.Errorf("❌    初始化MinIO客户端失败: %v", err)
	}

	// 测试连接
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = minioClient.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("❌    无法连接到MinIO: %v", err)
	}

	log.Println("✅    MinIO连接成功")
	return nil
}

func getImageFromMinIO(identifier string) (string, error) {
    // 先检查对象是否存在，避免无谓的临时文件创建
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    if _, err := minioClient.StatObject(ctx, config.MinIO.Bucket, identifier, minio.StatObjectOptions{}); err != nil {
        return "", fmt.Errorf("图像不存在: %v", err) // 直接返回，不创建临时文件
    }
    // 创建临时文件（用于存储下载的图片）
    tmpFile, err := os.CreateTemp(config.CacheDir, "minio-*.tmp")
    if err != nil {
        return "", fmt.Errorf("创建临时文件失败: %v", err)
    }
    tmpFilePath := tmpFile.Name()  // 保存临时文件路径
    tmpFile.Close()  // 立即关闭文件，稍后通过路径写入

    log.Printf("从MinIO下载: bucket=%s, key=%s, 临时文件=%s",
        config.MinIO.Bucket, identifier, tmpFilePath)

    // 从 MinIO 下载文件
    ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    object, err := minioClient.GetObject(ctx, config.MinIO.Bucket, identifier, minio.GetObjectOptions{})
    if err != nil {
        return "", fmt.Errorf("从MinIO获取对象失败: %v", err)
    }
    defer object.Close()

    // 检查对象是否存在
    stat, err := object.Stat()
    if err != nil {
        return "", fmt.Errorf("获取MinIO对象状态失败: %v", err)
    }

    if stat.Size == 0 {
        return "", errors.New("对象为空")
    }

    // 将 MinIO 文件写入临时文件
    file, err := os.OpenFile(tmpFilePath, os.O_WRONLY, 0644)
    if err != nil {
        return "", fmt.Errorf("打开临时文件进行写入失败: %v", err)
    }
    defer file.Close()

    if _, err := io.Copy(file, object); err != nil {
        return "", fmt.Errorf("从MinIO下载对象失败: %v", err)
    }

    log.Printf("成功从MinIO下载: %s -> %s", identifier, tmpFilePath)
    return tmpFilePath, nil
}

var identifierLocks sync.Map // key: identifier, value: *sync.Mutex
func getImagePath(identifier string) ([]byte, error) {
    muInterface, _ := identifierLocks.LoadOrStore(identifier, &sync.Mutex{})
    mu := muInterface.(*sync.Mutex)
    mu.Lock()
    defer mu.Unlock()

    // 如果 readMinIO 为 false，直接从本地读取
    if !config.ReadMinIO {
        localPath := filepath.Join(config.ImageDir, identifier)
        imgData, err := os.ReadFile(localPath)
        if err != nil {
            return nil, fmt.Errorf("本地图片不存在: %v", err)
        }
        return imgData, nil
    }

    // 检查缓存
    if cached, _ := cacheManager.isCached(identifier); cached {
        log.Printf("从缓存加载图像: %s", identifier)
        return cacheManager.getFromRedis(generateCacheKey(identifier))
    }

    // 从 MinIO 下载
    tmpFilePath, err := getImageFromMinIO(identifier)
    if err != nil {
        // 明确检查是否为 "未找到" 错误
        if strings.Contains(err.Error(), "The specified key does not exist") {
            return nil, fmt.Errorf("图像不存在: %s", identifier)
        }
        return nil, fmt.Errorf("MinIO下载失败: %v", err)
    }
    defer os.Remove(tmpFilePath) // 确保临时文件最终被删除

    // 读取临时文件内容
    imgData, err := os.ReadFile(tmpFilePath)
    if err != nil {
        return nil, fmt.Errorf("读取临时文件失败: %v", err)
    }

    // 只有成功获取图像数据后，才写入缓存
    cacheKey := generateCacheKey(identifier)
    if err := redisClient.Set(context.Background(), cacheKey, imgData, cacheManager.redisTTL).Err(); err != nil {
        log.Printf("警告: Redis缓存写入失败（但图像有效）: %v", err)
    } else {
        // 仅在 Redis 写入成功时创建本地键文件
        cachePath := cacheManager.getCachePath(cacheKey)
        if err := os.WriteFile(cachePath, []byte(""), 0644); err != nil {
            log.Printf("警告: 本地键文件创建失败: %v", err)
        }
    }

    return imgData, nil
}


func main() {
    // 初始化libvips（线程安全）
    vipsInit.Do(func() {
        vips.Startup(nil)
    })
    defer vips.Shutdown()

    // 设置Gin模式
    gin.SetMode(gin.ReleaseMode)
    r := gin.Default()

    // CORS中间件
    r.Use(func(c *gin.Context) {
        origin := c.Request.Header.Get("Origin")
        allowOrigin := ""

        // 检查允许的来源
        if len(config.CORS.AllowOrigins) > 0 {
            for _, o := range config.CORS.AllowOrigins {
                if o == "*" || o == origin {
                    allowOrigin = o
                    break
                }
            }
        } else {
            allowOrigin = "*"
        }

        if allowOrigin != "" {
            c.Header("Access-Control-Allow-Origin", allowOrigin)
            if allowOrigin != "*" && config.CORS.AllowCredentials {
                c.Header("Access-Control-Allow-Credentials", "true")
            }
        }

        // 设置允许的方法
        methods := "GET, OPTIONS"
        if len(config.CORS.AllowMethods) > 0 {
            methods = strings.Join(config.CORS.AllowMethods, ", ")
        }
        c.Header("Access-Control-Allow-Methods", methods)

        // 设置允许的头部
        headers := "Accept, Content-Type"
        if len(config.CORS.AllowHeaders) > 0 {
            headers = strings.Join(config.CORS.AllowHeaders, ", ")
        }
        c.Header("Access-Control-Allow-Headers", headers)

        // 处理OPTIONS请求
        if c.Request.Method == "OPTIONS" {
            c.AbortWithStatus(204)
            return
        }
        c.Next()
    })

    // 预编译IIIF 3.0标准正则
    iiifRegex := regexp.MustCompile(
        `^(.*?)/` + // identifier (group 1)
        `(full|square|\d+,\d+,\d+,\d+|pct:\d+,\d+,\d+,\d+)/` + // region (group 2)
        `(full|max|\d+,|,\d+|\d+,\d+|!?\d+,\d+|\^\d+,\d+|pct:\d+)/` + // size (group 3)
        `(!?\d+)/` + // rotation (group 4)
        `(default|color|gray|bitonal)\.` + // quality (group 5)
        `(jpg|png|webp|gif|tif)$`, // format (group 6)
    )

    // 基础路由
    r.GET("/", ginHomeHandler)
    r.GET("/health", ginHealthHandler)
    r.GET("/status", ginStatusHandler)

    // IIIF路由处理
    r.GET(fmt.Sprintf("/iiif/%s/*path", config.Version), func(c *gin.Context) {
        // 获取并清理路径
        rawPath := c.Param("path")
        cleanedPath := filepath.ToSlash(filepath.Clean(rawPath))

        // 验证路径规范
        if cleanedPath != rawPath {
            sendIIIFError(c, 400, "InvalidPath", "URL路径包含多余分隔符")
            return
        }

        // 解码URL编码
        decodedPath, err := url.PathUnescape(cleanedPath)
        if err != nil {
            sendIIIFError(c, 400, "InvalidEncoding", "URL解码失败")
            return
        }

        // 处理info.json请求
        if strings.HasSuffix(decodedPath, "info.json") {
            identifier := strings.TrimSuffix(decodedPath, "/info.json")
            ginMinioInfoHandler(c, identifier)
            return
        }

        // 验证图像请求格式
        if !iiifRegex.MatchString(decodedPath) {
            sendIIIFError(c, 400, "InvalidRequest", "URL格式不符合IIIF规范")
            return
        }

        // 提取参数
        matches := iiifRegex.FindStringSubmatch(decodedPath)
        req := IIIFRequest{
            Identifier: matches[1],
            Region:     matches[2],
            Size:       matches[3],
            Rotation:   matches[4],
            Quality:    matches[5],
            Format:     matches[6],
        }

        // 处理图像请求
        ginImageHandler(c, req)
    })

    // 启动服务器
    addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
    log.Printf("Starting IIIF server at %s (IIIF version %s)", addr, config.Version)

    if config.EnableHTTPS {
        if config.CertFile == "" || config.KeyFile == "" {
            log.Fatal("HTTPS enabled but missing cert/key files")
        }
        log.Fatal(r.RunTLS(addr, config.CertFile, config.KeyFile))
    } else {
        log.Fatal(r.Run(addr))
    }
}

func ginHomeHandler(c *gin.Context) {
    c.Header("Content-Type", "text/html")
    c.String(200, fmt.Sprintf(`
<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s - IIIF 图像服务器</title>
    <style>
        :root {
            --primary-color: #3498db;
            --secondary-color: #2980b9;
            --background-color: #f8f9fa;
            --text-color: #333;
            --border-color: #dee2e6;
        }
        body {
            font-family: 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
            line-height: 1.6;
            color: var(--text-color);
            background-color: var(--background-color);
            margin: 0;
            padding: 20px;
        }
        .container {
            max-width: 1000px;
            margin: 0 auto;
            background: white;
            padding: 30px;
            border-radius: 8px;
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        h1 {
            color: var(--primary-color);
            margin-top: 0;
            border-bottom: 2px solid var(--border-color);
            padding-bottom: 10px;
        }
        h2 {
            color: var(--secondary-color);
            margin-top: 25px;
        }
        .endpoint {
            background: white;
            border: 1px solid var(--border-color);
            border-left: 4px solid var(--primary-color);
            padding: 15px;
            margin: 15px 0;
            border-radius: 4px;
            transition: all 0.3s ease;
        }
        .endpoint:hover {
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
            transform: translateY(-2px);
        }
        code {
            background: #f5f5f5;
            padding: 2px 6px;
            border-radius: 3px;
            font-family: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
            color: #d63384;
        }
        .badge {
            display: inline-block;
            padding: 3px 7px;
            background: var(--primary-color);
            color: white;
            border-radius: 3px;
            font-size: 0.8em;
            margin-left: 10px;
        }
        .features {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
            gap: 15px;
            margin: 20px 0;
        }
        .feature-card {
            background: white;
            border: 1px solid var(--border-color);
            padding: 15px;
            border-radius: 5px;
        }
        a {
            color: var(--primary-color);
            text-decoration: none;
        }
        a:hover {
            text-decoration: underline;
        }
        .footer {
            margin-top: 30px;
            padding-top: 20px;
            border-top: 1px solid var(--border-color);
            font-size: 0.9em;
            color: #6c757d;
        }
        @media (max-width: 768px) {
            .container {
                padding: 15px;
            }
            .features {
                grid-template-columns: 1fr;
            }
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>IIIF 图像服务器 <span class="badge">version : %s</span></h1>

        <p>本服务器实现了 <a href="https://iiif.io/api/image/3.0/" target="_blank">IIIF Image API 3.0</a> 规范，提供标准化的图像访问和处理服务。</p>

        <h2>API 端点</h2>
        <div class="endpoint">
            <strong><code>GET /{identifier}/info.json</code></strong>
            <p>获取图像的元数据信息，包括尺寸、可用格式和质量选项等。</p>
        </div>

        <div class="endpoint">
            <strong><code>GET /{identifier}/{region}/{size}/{rotation}/{quality}.{format}</code></strong>
            <p>动态处理并返回图像，支持多种处理参数：</p>
            <ul>
                <li><strong>region</strong>: 图像区域 (full, square, x,y,w,h, pct:x,y,w,h)</li>
                <li><strong>size</strong>: 尺寸调整 (full, max, w,h, pct:n, !w,h, ^w,h)</li>
                <li><strong>rotation</strong>: 旋转角度 (0, 90, 180, 270)</li>
                <li><strong>quality</strong>: 质量 (default, color, gray, bitonal)</li>
                <li><strong>format</strong>: 格式 (jpg, png, webp, gif)</li>
            </ul>
        </div>

        <h2>支持的功能</h2>
        <div class="features">
            <div class="feature-card">
                <h3>图像处理</h3>
                <ul>
                    <li>区域裁剪</li>
                    <li>尺寸调整</li>
                    <li>旋转和翻转</li>
                    <li>质量转换</li>
                </ul>
            </div>
            <div class="feature-card">
                <h3>性能优化</h3>
                <ul>
                    <li>Redis 缓存</li>
                    <li>多级缓存策略</li>
                    <li>并发处理</li>
                </ul>
            </div>
            <div class="feature-card">
                <h3>存储支持</h3>
                <ul>
                    <li>本地文件系统</li>
                    <li>MinIO 对象存储</li>
                    <li>混合存储模式</li>
                </ul>
            </div>
        </div>

        <h2>使用示例</h2>
        <div class="endpoint">
            <strong>获取图像信息</strong>
            <p><code>%s/iiif/%s/sample-image/info.json</code></p>
        </div>
        <div class="endpoint">
            <strong>获取缩略图 (300x300)</strong>
            <p><code>%s/iiif/%s/sample-image/full/^300,300/0/default.jpg</code></p>
        </div>

        <div class="footer">
            <p>服务器版本: %s | 启动时间: %s | 运行时间: %s</p>
            <p>Go 版本: %s | CPU 核心: %d | Goroutines: %d</p>
        </div>
    </div>
</body>
</html>
    `,
    config.Host,  // 标题
    config.Version,  // 版本号
    config.Host, config.Version,  // 示例URL
    config.Host, config.Version,  // 示例URL
    config.Version,  // 页脚信息
    startTime.Format("2006-01-02 15:04:05"),  // 启动时间
    time.Since(startTime).Round(time.Second).String(),  // 运行时间
    runtime.Version(),  // Go版本
    runtime.NumCPU(),  // CPU核心
    runtime.NumGoroutine()))  // Goroutines数量
}

func ginHealthHandler(c *gin.Context) {
	c.JSON(200, gin.H{
		"status": "success",
		"time":   time.Now().Format(time.RFC3339),
	})
}

func ginStatusHandler(c *gin.Context) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	 // 缓存大小统计
    cacheSize := 0
    cache.Range(func(_, value interface{}) bool {
        if data, ok := value.([]byte); ok {
            cacheSize += len(data)
        }
        return true
    })
	imageCount := 0
	filepath.Walk(config.ImageDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" ||  ext == ".tiff" || ext == ".webp" || ext == ".gif" {
				imageCount++
			}
		}
		return nil
	})
	status := ServerStatus{
		StartTime:    startTime,
		Uptime:       time.Since(startTime).String(),
		GoVersion:    runtime.Version(),
		NumCPU:       runtime.NumCPU(),
		NumGoroutine: runtime.NumGoroutine(),
		MemoryStats:  memStats,
		CacheSize:    cacheSize,
        ImageCount:   imageCount,
	}
	c.JSON(200, status)
}

func ginMinioInfoHandler(c *gin.Context, identifier string) {
    log.Printf("获取MinIO图像信息: %s", identifier)

    // 获取图像数据
    imgData, err := getImagePath(identifier)
    if err != nil {
        log.Printf("获取图像失败: %v", err)
        if strings.Contains(err.Error(), "未找到") {
            sendIIIFError(c, 404, "NotFound", err.Error())
        } else {
            sendIIIFError(c, 500, "InternalServerError", err.Error())
        }
        return
    }


    // 创建临时文件处理图像
    tmpFile, err := os.CreateTemp("", "iiif-info-tmp-*")
    if err != nil {
        log.Printf("创建临时文件失败: %v", err)
        sendIIIFError(c, 500, "InternalServerError", "创建临时文件失败")
        return
    }
    defer func() {
        tmpFile.Close()
        if err := os.Remove(tmpFile.Name()); err != nil {
            log.Printf("删除临时文件失败: %v", err)
        }
    }()

    // 写入图像数据到临时文件
    if _, err := tmpFile.Write(imgData); err != nil {
        log.Printf("写入临时文件失败: %v", err)
        sendIIIFError(c, 500, "InternalServerError", "写入临时文件失败")
        return
    }
    if err := tmpFile.Sync(); err != nil {
        log.Printf("同步临时文件失败: %v", err)
    }

    // 打开图片文件
    img, err := vips.NewImageFromFile(tmpFile.Name())
    if err != nil {
        log.Printf("读取图像失败: %v", err)
        sendIIIFError(c, 500, "InternalServerError", fmt.Sprintf("读取图像错误: %v", err))
        return
    }
    defer img.Close()

    // 获取图片尺寸信息
    width := img.Width()
    height := img.Height()

    // 构建IIIF info.json响应
    info := IIIFInfo{
        Context:        "http://iiif.io/api/image/3/context.json",
        ID:             fmt.Sprintf("http://%s:%d/iiif/%s/%s", config.Host, config.Port,config.Version , strings.Trim(identifier, "/")),
//         ID:             fmt.Sprintf("http://t677cea3.natappfree.cc/iiif/V1/%s", strings.Trim(identifier, "/")),
        Type:           "sc:Manifest",
        Protocol:       "http://iiif.io/api/image",
        Width:          width,
        Height:         height,
        Profile: []string{
            "http://iiif.io/api/image/3/level2.json",  // 主合规级别
            "http://iiif.io/api/image/3/profiles/level2.json", // 兼容性格式
        },

        Tiles: []Tile{
            {
                Width:        512,
                ScaleFactors: []int{1, 2, 4, 8},
            },
        },
        Sizes: []Size{
            {Width: width, Height: height},
            {Width: width / 2, Height: height / 2},
            {Width: width / 4, Height: height / 4},
        },

        ExtraFormats:   []string{"jpg", "png", "webp", "gif"},
        ExtraQualities: []string{"default", "color", "gray", "bitonal"},
        ExtraFeatures:  []string{
            "regionByPct",       // 百分比区域
            "regionSquare",      // 方形区域
            "sizeByWhListed",    // 明确尺寸
            "sizeByPct",         // 百分比缩放
            "sizeByW",           // 按宽度缩放
            "sizeByH",           // 按高度缩放
            "sizeByConfinedWh",  // 限制框缩放
            "sizeByDistortedWh", // 非等比缩放 (^语法)
            "rotationBy90s",  // 明确声明仅支持90度倍数旋转
//             "mirroring",          // 支持!翻转
            "regionSquare",
        },
    }

    // 返回JSON响应
    c.Header("Content-Type", "application/json")
    if err := json.NewEncoder(c.Writer).Encode(info); err != nil {
        log.Printf("JSON编码失败: %v", err)
    }
}

func ginImageHandler(c *gin.Context, req IIIFRequest) {
    // 验证参数有效性
    if !isValidFormat(req.Format) {
        sendIIIFError(c, 400, "InvalidRequest",
            fmt.Sprintf("Unsupported format: %s. Supported: jpg, png, webp, gif", req.Format))
        return
    }

    if !isValidQuality(req.Quality) {
        sendIIIFError(c, 400, "InvalidRequest",
            fmt.Sprintf("Unsupported quality: %s. Supported: default, color, gray, bitonal", req.Quality))
        return
    }

    // 获取图像数据
    imgData, err := getImagePath(req.Identifier)
    if err != nil {
        log.Printf("获取图像失败: %v", err)
        if strings.Contains(err.Error(), "未找到") {
            sendIIIFError(c, 404, "NotFound", err.Error())
        } else {
            sendIIIFError(c, 500, "InternalServerError", err.Error())
        }
        return
    }

    // 创建临时文件处理图像
    tmpFile, err := os.CreateTemp("", "iiif-tmp-*")
    if err != nil {
        sendIIIFError(c, 500, "InternalServerError", "创建临时文件失败")
        return
    }
    defer os.Remove(tmpFile.Name())
    defer tmpFile.Close()

    if _, err := tmpFile.Write(imgData); err != nil {
        sendIIIFError(c, 500, "InternalServerError", "写入临时文件失败")
        return
    }

    // 处理图像
    img, err := processImage(tmpFile.Name(), req)
    if err != nil {
        log.Printf("图像处理失败: %v", err)
        sendIIIFError(c, 400, "InvalidRequest", fmt.Sprintf("图像处理失败: %v", err))
        return
    }
    defer img.Close()

    // 导出处理后的图片
    var imageBytes []byte
    var exportErr error

    switch req.Format {
    case "jpg", "jpeg":
        params := vips.NewJpegExportParams()
        params.Quality = 85
        imageBytes, _, exportErr = img.ExportJpeg(params)
    case "png":
        params := vips.NewPngExportParams()
        imageBytes, _, exportErr = img.ExportPng(params)
    case "webp":
        params := vips.NewWebpExportParams()
        imageBytes, _, exportErr = img.ExportWebp(params)
    case "gif":
        params := vips.NewGifExportParams()
        imageBytes, _, exportErr = img.ExportGIF(params)
    default:
        exportErr = fmt.Errorf("unsupported format: %s", req.Format)
    }

    if exportErr != nil {
        sendIIIFError(c, 500, "InternalError", fmt.Sprintf("导出失败: %v", exportErr))
        return
    }

    // 返回处理后的图片
    c.Data(200, "image/"+req.Format, imageBytes)
}

// 辅助函数 - 验证格式是否支持
func isValidFormat(format string) bool {
    switch format {
    case "jpg", "jpeg", "png", "webp", "gif":
        return true
    default:
        return false
    }
}

// 辅助函数 - 验证质量参数是否支持
func isValidQuality(quality string) bool {
    switch quality {
    case "default", "color", "gray", "bitonal":
        return true
    default:
        return false
    }
}


func processImage(path string, req IIIFRequest) (*vips.ImageRef, error) {
    img, err := vips.NewImageFromFile(path)
    if err != nil {
        return nil, fmt.Errorf("读取图像错误: %v", err)
    }

    if err := applyRegion(img, req.Region); err != nil {
        return nil, fmt.Errorf("区域处理失败: %v", err)
    }

    if err := applySize(img, req.Size); err != nil {
        return nil, fmt.Errorf("尺寸处理失败: %v", err)
    }

    if err := applyRotation(img, req.Rotation); err != nil {
        return nil, fmt.Errorf("旋转处理失败: %v", err)
    }

    if err := applyQuality(img, req.Quality); err != nil {
        return nil, fmt.Errorf("质量处理失败: %v", err)
    }

    return img, nil
}

func applyRegion(img *vips.ImageRef, region string) error {
	if region == "full" {
		return nil
	}

	width := img.Width()
	height := img.Height()

	var x, y, w, h int

	if strings.HasPrefix(region, "pct:") {
		parts := strings.Split(region[4:], ",")
		if len(parts) != 4 {
			return errors.New("区域格式无效")
		}

		xPct, err1 := strconv.ParseFloat(parts[0], 64)
		yPct, err2 := strconv.ParseFloat(parts[1], 64)
		wPct, err3 := strconv.ParseFloat(parts[2], 64)
		hPct, err4 := strconv.ParseFloat(parts[3], 64)

		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return errors.New("区域值无效")
		}

		x = int(float64(width) * xPct / 100)
		y = int(float64(height) * yPct / 100)
		w = int(float64(width) * wPct / 100)
		h = int(float64(height) * hPct / 100)
	} else if region == "square" {
		if width > height {
			x = (width - height) / 2
			w = height
			h = height
		} else {
			y = (height - width) / 2
			w = width
			h = width
		}
	} else {
		parts := strings.Split(region, ",")
		if len(parts) != 4 {
			return errors.New("区域格式无效")
		}

		var err error
		x, err = strconv.Atoi(parts[0])
		if err != nil {
			return errors.New("区域值无效")
		}
		y, err = strconv.Atoi(parts[1])
		if err != nil {
			return errors.New("区域值无效")
		}
		w, err = strconv.Atoi(parts[2])
		if err != nil {
			return errors.New("区域值无效")
		}
		h, err = strconv.Atoi(parts[3])
		if err != nil {
			return errors.New("区域值无效")
		}
	}

	if x < 0 || y < 0 || w <= 0 || h <= 0 || x+w > width || y+h > height {
        return fmt.Errorf("区域超出边界: x=%d, y=%d, w=%d, h=%d (图像尺寸: %dx%d)",
            x, y, w, h, width, height)	}

	return img.ExtractArea(x, y, w, h)
}

func applySize(img *vips.ImageRef, size string) error {
    log.Printf("应用尺寸: %s", size)

    width := img.Width()
    height := img.Height()
    if size == "full" {
        // full 直接返回原始尺寸（不检查 maxPixels）
        if width*height > config.MaxPixels {
            return fmt.Errorf("请求完整尺寸 (%dx%d) 超过服务器限制 (%d 像素)",
                width, height, config.MaxPixels)
        }
        return nil
    }

    if size == "max" {
        // max 需要检查是否超过 maxPixels
        if width*height <= config.MaxPixels {
            return nil // 原始尺寸在限制内，等同于 full
        }
        // 超过限制时，计算缩小比例
        scale := math.Sqrt(float64(config.MaxPixels) / float64(width*height))
        newWidth := int(float64(width) * scale)
        newHeight := int(float64(height) * scale)
        log.Printf("自动缩放到最大允许尺寸: %dx%d -> %dx%d",
            width, height, newWidth, newHeight)
        return img.Resize(float64(newWidth)/float64(width), vips.KernelLanczos3)
    }
    if width <= 0 || height <= 0 {
        return errors.New("图像尺寸无效")
    }

    var newWidth, newHeight int
    var err error

    // 处理百分比缩放 (sizeByPct)
    if strings.HasPrefix(size, "pct:") {
        scale, err := strconv.ParseFloat(size[4:], 64)
        if err != nil {
            return errors.New("尺寸百分比值无效")
        }
        newWidth = int(float64(width) * scale / 100)
        newHeight = int(float64(height) * scale / 100)

    // 处理按宽度缩放 (sizeByW)
    } else if strings.HasSuffix(size, ",") {
        widthStr := strings.TrimSuffix(size, ",")
        newWidth, err = strconv.Atoi(widthStr)
        if err != nil {
            return errors.New("宽度值无效")
        }
        newHeight = int(float64(height) * (float64(newWidth) / float64(width)))

    // 处理按高度缩放 (sizeByH)
    } else if strings.HasPrefix(size, ",") {
        heightStr := strings.TrimPrefix(size, ",")
        newHeight, err = strconv.Atoi(heightStr)
        if err != nil {
            return errors.New("高度值无效")
        }
        newWidth = int(float64(width) * (float64(newHeight) / float64(height)))

    // 处理限定框缩放 (sizeByConfinedWh)
    } else if strings.HasPrefix(size, "!") {
        parts := strings.Split(size[1:], ",")
        if len(parts) != 2 {
            return errors.New("尺寸格式无效")
        }

        maxWidth, err := strconv.Atoi(parts[0])
        if err != nil {
            return errors.New("最大宽度值无效")
        }
        maxHeight, err := strconv.Atoi(parts[1])
        if err != nil {
            return errors.New("最大高度值无效")
        }

        ratio := min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
        newWidth = int(float64(width) * ratio)
        newHeight = int(float64(height) * ratio)

    // 处理最佳匹配缩放 (sizeByWhListed)
    } else if strings.HasPrefix(size, "^") {
        parts := strings.Split(size[1:], ",")
        if len(parts) != 2 {
            return errors.New("尺寸格式无效")
        }

        targetWidth, err := strconv.Atoi(parts[0])
        if err != nil {
            return errors.New("目标宽度值无效")
        }
        targetHeight, err := strconv.Atoi(parts[1])
        if err != nil {
            return errors.New("目标高度值无效")
        }

        ratio := max(float64(targetWidth)/float64(width), float64(targetHeight)/float64(height))
        newWidth = int(float64(width) * ratio)
        newHeight = int(float64(height) * ratio)

    // 处理标准 w,h 格式
    } else {
        parts := strings.Split(size, ",")
        switch len(parts) {
        case 1: // 单个数字 (隐式 sizeByW)
            newWidth, err = strconv.Atoi(parts[0])
            if err != nil {
                return errors.New("宽度值无效")
            }
            newHeight = int(float64(height) * (float64(newWidth) / float64(width)))
        case 2: // 明确指定 w,h
            newWidth, err = strconv.Atoi(parts[0])
            if err != nil {
                return errors.New("宽度值无效")
            }
            newHeight, err = strconv.Atoi(parts[1])
            if err != nil {
                return errors.New("高度值无效")
            }
        default:
            return errors.New("尺寸格式无效")
        }
    }

    // 验证计算后的尺寸
    if newWidth <= 0 || newHeight <= 0 {
        return errors.New("计算得到的尺寸无效")
    }

    // 检查最大像素限制
    if newWidth*newHeight > config.MaxPixels {
        return fmt.Errorf("请求的尺寸 %dx%d 超过最大允许值 (%d 像素)",
            newWidth, newHeight, config.MaxPixels)
    }

    log.Printf("将图像从 %dx%d 缩放为 %dx%d", width, height, newWidth, newHeight)

    // 计算缩放比例并应用
    scale := float64(newWidth) / float64(width)
    return img.Resize(scale, vips.KernelLanczos3)
}


func applyRotation(img *vips.ImageRef, rotation string) error {
	if rotation == "0" {
		return nil
	}

	angle, err := strconv.ParseFloat(rotation, 64)
	if err != nil {
		return errors.New("旋转值无效")
	}

	var flip bool
	if strings.HasPrefix(rotation, "!") {
		flip = true
		angle, err = strconv.ParseFloat(rotation[1:], 64)
		if err != nil {
			return errors.New("旋转值无效")
		}
	}

	if flip {
		if err := img.Flip(vips.DirectionHorizontal); err != nil {
			return err
		}
	}
	// 注意：govips 可能不支持任意角度旋转，这里我们只支持90度的倍数
	var vipsAngle vips.Angle
	switch {
	case angle == 90:
		vipsAngle = vips.Angle90
	case angle == 180:
		vipsAngle = vips.Angle180
	case angle == 270:
		vipsAngle = vips.Angle270
	default:
        return fmt.Errorf("仅支持0度(不旋转)、90、180和270度旋转，请求角度: %v", angle)
	}

	return img.Rotate(vipsAngle)
}

func applyQuality(img *vips.ImageRef, quality string) error {
	switch quality {
	case "default", "color":
		return nil
	case "gray":
		return img.ToColorSpace(vips.InterpretationBW)
	case "bitonal":
		// 转换为灰度
		if err := img.ToColorSpace(vips.InterpretationBW); err != nil {
			return err
		}
		// 简单阈值处理
		return img.Linear([]float64{1.0}, []float64{-128.0}) // 将128以下的像素设为0，以上的设为255
	default:
		return nil
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}