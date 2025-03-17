package speedtester

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"gopkg.in/yaml.v3"
)

type Config struct {
	ConfigPaths  		string
	FilterRegex  		string
	ServerURL    		string
	DownloadSize 		int
	UploadSize   		int
	Timeout      		time.Duration
	Concurrent   		int
	ExtraConnectURL 	[]string
	ExtraDownloadURL	string
}

type SpeedTester struct {
	config *Config
}

func New(config *Config) *SpeedTester {
	if config.Concurrent <= 0 {
		config.Concurrent = 1
	}
	if config.DownloadSize <= 0 {
		config.DownloadSize = 100 * 1024 * 1024
	}
	if config.UploadSize <= 0 {
		config.UploadSize = 10 * 1024 * 1024
	}
	return &SpeedTester{
		config: config,
	}
}

type CProxy struct {
	constant.Proxy
	Config map[string]any
}

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []map[string]any          `yaml:"proxies"`
}

func (st *SpeedTester) LoadProxies() (map[string]*CProxy, error) {
	allProxies := make(map[string]*CProxy)

	for _, configPath := range strings.Split(st.config.ConfigPaths, ",") {
		var body []byte
		var err error
		if strings.HasPrefix(configPath, "http") {
			var resp *http.Response
			resp, err = http.Get(configPath)
			if err != nil {
				log.Warnln("failed to fetch config: %s", err)
				continue
			}
			body, err = io.ReadAll(resp.Body)
		} else {
			body, err = os.ReadFile(configPath)
		}
		if err != nil {
			log.Warnln("failed to read config: %s", err)
			continue
		}

		rawCfg := &RawConfig{
			Proxies: []map[string]any{},
		}
		if err := yaml.Unmarshal(body, rawCfg); err != nil {
			return nil, err
		}
		proxies := make(map[string]*CProxy)
		proxiesConfig := rawCfg.Proxies
		providersConfig := rawCfg.Providers

		for i, config := range proxiesConfig {
			proxy, err := adapter.ParseProxy(config)
			if err != nil {
				return nil, fmt.Errorf("proxy %d: %w", i, err)
			}

			if _, exist := proxies[proxy.Name()]; exist {
				return nil, fmt.Errorf("proxy %s is the duplicate name", proxy.Name())
			}
			proxies[proxy.Name()] = &CProxy{Proxy: proxy, Config: config}
		}
		for name, config := range providersConfig {
			if name == provider.ReservedName {
				return nil, fmt.Errorf("can not defined a provider called `%s`", provider.ReservedName)
			}
			pd, err := provider.ParseProxyProvider(name, config)
			if err != nil {
				return nil, fmt.Errorf("parse proxy provider %s error: %w", name, err)
			}
			if err := pd.Initial(); err != nil {
				return nil, fmt.Errorf("initial proxy provider %s error: %w", pd.Name(), err)
			}
			for _, proxy := range pd.Proxies() {
				proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = &CProxy{Proxy: proxy}
			}
		}
		for k, p := range proxies {
			switch p.Type() {
			case constant.Shadowsocks, constant.ShadowsocksR, constant.Snell, constant.Socks5, constant.Http,
				constant.Vmess, constant.Vless, constant.Trojan, constant.Hysteria, constant.Hysteria2,
				constant.WireGuard, constant.Tuic, constant.Ssh:
			default:
				continue
			}
			if _, ok := allProxies[k]; !ok {
				allProxies[k] = p
			}
		}
	}

	filterRegexp := regexp.MustCompile(st.config.FilterRegex)
	filteredProxies := make(map[string]*CProxy)
	for name := range allProxies {
		if filterRegexp.MatchString(name) {
			filteredProxies[name] = allProxies[name]
		}
	}
	return filteredProxies, nil
}

func (st *SpeedTester) TestProxies(proxies map[string]*CProxy, fn func(result *Result)) {
	for name, proxy := range proxies {
		fn(st.testProxy(name, proxy))
	}
}

type testJob struct {
	name  string
	proxy *CProxy
}

type Result struct {
	ProxyName     			string         `json:"proxy_name"`
	ProxyType     			string         `json:"proxy_type"`
	ProxyConfig  			map[string]any `json:"proxy_config"`
	Latency       			time.Duration  `json:"latency"`
	Jitter       			time.Duration  `json:"jitter"`
	PacketLoss    			float64        `json:"packet_loss"`
	DownloadSize  			float64        `json:"download_size"`
	DownloadTime  			time.Duration  `json:"download_time"`
	DownloadSpeed 			float64        `json:"download_speed"`
	UploadSize   			float64        `json:"upload_size"`
	UploadTime   			time.Duration  `json:"upload_time"`
	UploadSpeed   			float64        `json:"upload_speed"`
	ExtraURLConnectivity	bool		   `json:extra_url_connectivity`
	ExtraURLOpenSpeed       float64        `json:"extra_url_open_speed"`
	ExtraDownloadSpeed		float64        `json:"extra_download_speed"`
}

func (r *Result) FormatDownloadSpeed() string {
	return formatSpeed(r.DownloadSpeed)
}

func (r *Result) FormatLatency() string {
	if r.Latency == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Latency.Milliseconds())
}

func (r *Result) FormatJitter() string {
	if r.Jitter == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%dms", r.Jitter.Milliseconds())
}

func (r *Result) FormatPacketLoss() string {
	return fmt.Sprintf("%.1f%%", r.PacketLoss)
}

func (r *Result) FormatUploadSpeed() string {
	return formatSpeed(r.UploadSpeed)
}

func formatSpeed(bytesPerSecond float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s", "TB/s"}
	unit := 0
	speed := bytesPerSecond
	for speed >= 1024 && unit < len(units)-1 {
		speed /= 1024
		unit++
	}
	return fmt.Sprintf("%.2f%s", speed, units[unit])
}

// getFileNameWithoutExt 从路径或 URL 中提取文件名并去掉后缀
func getFileNameWithoutExt(input string) (string, error) {
    // 解析 URL
    parsedURL, err := url.Parse(input)
    if err == nil && parsedURL.Scheme != "" {
        // 如果是有效的 URL，使用 URL 的 Path
        input = parsedURL.Path
    }

    // 获取文件名
    fileName := filepath.Base(input) // 获取路径中的最后一部分

    // 去掉文件后缀
    fileNameWithoutExt := strings.TrimSuffix(fileName, filepath.Ext(fileName))

    return fileNameWithoutExt, nil
}

func existConnectivityProblem(latencyResultMap map[string]*latencyResult) bool {
	if len(latencyResultMap) == 0 {
		return false
	}
	for _, data := range latencyResultMap {
        if data.packetLoss == 100 {
			return true
		}
    }
	return false
}

func (st *SpeedTester) testProxy(name string, proxy *CProxy) *Result {
	fileName, _ := getFileNameWithoutExt(st.config.ConfigPaths)
	result := &Result{
		ProxyName:   fileName + "_" + name,
		ProxyType:   proxy.Type().String(),
		ProxyConfig: proxy.Config,
	}

	// 1. 首先进行延迟测试
	latencyResult := st.testLatency(proxy)
	result.Latency = latencyResult.avgLatency
	result.Jitter = latencyResult.jitter
	result.PacketLoss = latencyResult.packetLoss

	// 如果延迟测试完全失败，直接返回
	if result.PacketLoss == 100 {
		return result
	}

	extraLatencyResult, extraOpenResult, extraDownloadResult := st.testExtraLatencyAndSpeed(proxy)
	if existConnectivityProblem(extraLatencyResult) {
		result.ExtraURLConnectivity = false
		return result
	} else {
		result.ExtraURLConnectivity = true
	}
	if extraOpenResult != nil {
		result.ExtraURLOpenSpeed = float64(extraOpenResult.bytes) / extraOpenResult.duration.Seconds()
	}
	if extraDownloadResult != nil {
		result.ExtraDownloadSpeed = float64(extraDownloadResult.bytes) / extraDownloadResult.duration.Seconds()
	}

	// 2. 并发进行下载和上传测试
	var wg sync.WaitGroup
	downloadResults := make(chan *downloadResult, st.config.Concurrent)

	// 计算每个并发连接的数据大小
	downloadChunkSize := st.config.DownloadSize / st.config.Concurrent
	uploadChunkSize := st.config.UploadSize / st.config.Concurrent

	// 启动下载测试
	for i := 0; i < st.config.Concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			downloadResults <- st.testDownload(proxy, fmt.Sprintf("%s/__down?bytes=%d", st.config.ServerURL, downloadChunkSize))
		}()
	}
	wg.Wait()

	uploadResults := make(chan *downloadResult, st.config.Concurrent)

	// 启动上传测试
	for i := 0; i < st.config.Concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uploadResults <- st.testUpload(proxy, uploadChunkSize)
		}()
	}
	wg.Wait()


	// 3. 汇总结果
	var totalDownloadBytes, totalUploadBytes int64
	var totalDownloadTime, totalUploadTime time.Duration
	var downloadCount, uploadCount int

	for i := 0; i < st.config.Concurrent; i++ {
		if dr := <-downloadResults; dr != nil {
			totalDownloadBytes += dr.bytes
			totalDownloadTime += dr.duration
			downloadCount++
		}
	}
	close(downloadResults)

	for i := 0; i < st.config.Concurrent; i++ {
		if ur := <-uploadResults; ur != nil {
			totalUploadBytes += ur.bytes
			totalUploadTime += ur.duration
			uploadCount++
		}
	}
	close(uploadResults)

	if downloadCount > 0 {
		result.DownloadSize = float64(totalDownloadBytes)
		result.DownloadTime = totalDownloadTime / time.Duration(downloadCount)
		result.DownloadSpeed = float64(totalDownloadBytes) / result.DownloadTime.Seconds()
	}
	if uploadCount > 0 {
		result.UploadSize = float64(totalUploadBytes)
		result.UploadTime = totalUploadTime / time.Duration(uploadCount)
		result.UploadSpeed = float64(totalUploadBytes) / result.UploadTime.Seconds()
	}

	return result
}

type latencyResult struct {
	avgLatency time.Duration
	jitter     time.Duration
	packetLoss float64
}

func (st *SpeedTester) testLatency(proxy constant.Proxy) *latencyResult {
	client := st.createClient(proxy)
	latencies := make([]time.Duration, 0, 6)
	failedPings := 0

	for i := 0; i < 6; i++ {
		time.Sleep(100 * time.Millisecond)

		start := time.Now()
		resp, err := client.Get(fmt.Sprintf("%s/__down?bytes=0", st.config.ServerURL))
		if err != nil {
			failedPings++
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			latencies = append(latencies, time.Since(start))
		} else {
			failedPings++
		}
	}

	return calculateLatencyStats(latencies, failedPings)
}

func (st *SpeedTester) testExtraLatencyAndSpeed(proxy constant.Proxy) (map[string]*latencyResult, *downloadResult, *downloadResult) {
	client := st.createClient(proxy)
	testTimes := 6
	var extraLatencyResult map[string]*latencyResult
	var extraOpenResult *downloadResult
	var extraDownloadResult *downloadResult

	totalDownloadBytes := int64(0)
	totalDownloadDuration := time.Duration(0)
	if len(st.config.ExtraConnectURL) > 0 {
		extraLatencyResult = make(map[string]*latencyResult, len(st.config.ExtraConnectURL))
		
		for _, url := range st.config.ExtraConnectURL {
			latencies := make([]time.Duration, 0, testTimes)
			failedPings := 0
			for i := 0; i < testTimes; i++ {
				time.Sleep(100 * time.Millisecond)
	
				start := time.Now()
				resp, err := client.Get(url)
				if err != nil {
					failedPings++
					continue
				}
				
				if resp.StatusCode == http.StatusOK {
					latencies = append(latencies, time.Since(start))
				} else {
					failedPings++
					continue
				}

				downloadBytes, _ := io.Copy(io.Discard, resp.Body)
				totalDownloadBytes += downloadBytes
				totalDownloadDuration += time.Since(start)

				resp.Body.Close()
			}
			extraLatencyResult[url] = calculateLatencyStats(latencies, failedPings)
			if extraLatencyResult[url].packetLoss == 100 {
				//如果连通性测试都不OK的话，也就不用继续了
				return extraLatencyResult, nil, nil
			}
		}
		if totalDownloadBytes > 0 {
			extraOpenResult = &downloadResult{
				bytes:    totalDownloadBytes,
				duration: totalDownloadDuration,
			}
		}
	}
	if st.config.ExtraDownloadURL != "" {
		extraDownloadResult = st.testDownload(proxy, st.config.ExtraDownloadURL)
	}
	

	return extraLatencyResult, extraOpenResult, extraDownloadResult
}

type downloadResult struct {
	bytes    int64
	duration time.Duration
}

func (st *SpeedTester) testDownload(proxy constant.Proxy, string url) *downloadResult {
	client := st.createClient(proxy)
	start := time.Now()

	resp, err := client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	downloadBytes, _ := io.Copy(io.Discard, resp.Body)

	return &downloadResult{
		bytes:    downloadBytes,
		duration: time.Since(start),
	}
}

func (st *SpeedTester) testUpload(proxy constant.Proxy, size int) *downloadResult {
	client := st.createClient(proxy)
	reader := NewZeroReader(size)

	start := time.Now()
	resp, err := client.Post(
		fmt.Sprintf("%s/__up", st.config.ServerURL),
		"application/octet-stream",
		reader,
	)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	return &downloadResult{
		bytes:    reader.WrittenBytes(),
		duration: time.Since(start),
	}
}

func (st *SpeedTester) createClient(proxy constant.Proxy) *http.Client {
	return &http.Client{
		Timeout: st.config.Timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &constant.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}
}

func calculateLatencyStats(latencies []time.Duration, failedPings int) *latencyResult {
	result := &latencyResult{
		packetLoss: float64(failedPings) / 6.0 * 100,
	}

	if len(latencies) == 0 {
		return result
	}

	// 计算平均延迟
	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	result.avgLatency = total / time.Duration(len(latencies))

	// 计算抖动
	var variance float64
	for _, l := range latencies {
		diff := float64(l - result.avgLatency)
		variance += diff * diff
	}
	variance /= float64(len(latencies))
	result.jitter = time.Duration(math.Sqrt(variance))

	return result
}
