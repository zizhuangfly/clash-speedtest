package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/faceair/clash-speedtest/speedtester"
	"github.com/metacubex/mihomo/log"
	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v3"
	"gopkg.in/yaml.v3"
)

var (
	configPathsConfig 			= flag.String("c", "", "config file path, also support http(s) url")
	filterRegexConfig 			= flag.String("f", ".+", "filter proxies by name, use regexp")
	serverURL        		    = flag.String("server-url", "https://speed.cloudflare.com", "server url")
	downloadSize      			= flag.Int("download-size", 50*1024*1024, "download size for testing proxies")
	uploadSize        			= flag.Int("upload-size", 20*1024*1024, "upload size for testing proxies")
	timeout           			= flag.Duration("timeout", time.Second*5, "timeout for testing proxies")
	concurrent        			= flag.Int("concurrent", 4, "download concurrent size")
	outputPath       			= flag.String("output", "./useable.yaml", "output config file path")
	goodOutputPath				= flag.String("good-output", "./good.yaml", "output good config file path")
	maxLatency        			= flag.Duration("max-latency", 800*time.Millisecond, "filter latency greater than this value")
	minSpeed         			= flag.Float64("min-speed", 0.1, "filter speed less than this value(unit: MB/s)")
	skipPaths		  			= flag.String("skip-paths", "", "filter unwanted yaml file if specify direcotry")
	extraConnectURL   			= flag.String("extra-connect-url", "", "must connect urls, ',' split multiple urls")
	extraDownloadURL  			= flag.String("extra-download-url", "", "extra speed test url, like google drive share files")
	openSpeedThreshold			= flag.Float64("open-speed-threshold", 0.1, "满足节点可用性的网站打开速度(单位: MB/s)")
	goodOpenSpeedThreshold		= flag.Float64("good-open-speed-threshold", 0.5, "确定为优质节点的网站打开速度(单位: MB/s)")
	goodDownloadSpeedThreshold	= flag.Float64("good-download-speed-threshold", 0.5, "确定为优质节点的资源下载速度(单位: MB/s)")
)

const (
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorReset  = "\033[0m"
)

func main() {
	flag.Parse()
	log.SetLevel(log.INFO)

	if *configPathsConfig == "" {
		log.Fatalln("please specify the configuration file")
	}
	config := speedtester.Config{
		//ConfigPaths:  		*configPathsConfig,
		FilterRegex:  		*filterRegexConfig,
		ServerURL:    		*serverURL,
		DownloadSize: 		*downloadSize,
		UploadSize:   		*uploadSize,
		Timeout:      		*timeout,
		Concurrent:   		*concurrent,
		ExtraDownloadURL: 	*extraDownloadURL,
	}
	if *extraConnectURL != "" {
		config.ExtraConnectURL = strings.Split(*extraConnectURL, ",")
	}

	actualPaths, _ := getAllConfigPath(*configPathsConfig, *skipPaths)
	if len(actualPaths) == 0 {
		log.Fatalln("cannot find yaml paths")
	}

	speedTester := speedtester.New(&config)

	// 主进度条配置
	mainBar := progressbar.NewOptions(len(actualPaths),
		progressbar.OptionSetDescription("📦 yaml测试进行中"),
		progressbar.OptionSetWidth(30),
		progressbar.OptionShowCount(),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "█",
			SaucerPadding: " ",
			BarStart:      "|",
			BarEnd:        "|",
		}),
		progressbar.OptionOnCompletion(func() { 
			fmt.Println("\n✅ 所有yaml文件测试完成！") 
		}),
	)
	results := make([]*speedtester.Result, 0)

	for _, actualPath := range actualPaths {
		config.ConfigPaths = actualPath
		mainBar.Describe("📦 " + actualPath)
		allProxies, err := speedTester.LoadProxies()
		if err != nil {
			log.Warnln("load proxies failed: %v, %v, ", actualPath, err)
		}
		subBar := createSubBar(len(allProxies))
		speedTester.TestProxies(allProxies, func(name string) {
			subBar.Describe("    ↳🛠️" + name)
		},
		func(result *speedtester.Result) {
			subBar.Add(1)
			if isProxyUsable(result) {
				results = append(results, result)
			} else {
				log.Infoln("%s is not useable", result.ProxyName)
			}
		})
		subBar.Finish()
		mainBar.Add(1)
	}
	mainBar.Finish()
	
	sort.Slice(results, func(i, j int) bool {
		if isProxyGood(results[i]) == isProxyGood(results[j]) {
			return results[i].DownloadSpeed > results[j].DownloadSpeed
		}
		return isProxyGood(results[i])
	})

	printResults(results)

	if *outputPath != "" || *goodOutputPath != "" {
		saveConfig(results)
	}
}

func isProxyUsable(result *speedtester.Result) bool {
	return (result.Latency <= *maxLatency || *maxLatency == 0) && result.ExtraURLConnectivity && 
	(result.ExtraURLOpenSpeed >= *openSpeedThreshold * 1024 * 1024 || *extraConnectURL == "") &&
	result.DownloadSpeed >= *minSpeed * 1024 * 1024 && 
	(result.ExtraDownloadSpeed >= *minSpeed * 1024 * 1024 || *extraDownloadURL == "")
}


func isProxyGood(result *speedtester.Result) bool {
	return isProxyUsable(result) && result.DownloadSpeed >= *goodDownloadSpeedThreshold &&
	(result.ExtraURLOpenSpeed >= *goodOpenSpeedThreshold || *extraConnectURL == "") &&
	(result.ExtraDownloadSpeed >= *goodDownloadSpeedThreshold || *extraDownloadURL == "")
}


func createSubBar(proxyNumber int) *progressbar.ProgressBar {
	// 子进度条带缩进
	subBar := progressbar.NewOptions(proxyNumber,
		progressbar.OptionSetWidth(20),
		progressbar.OptionSetDescription("    ↳🛠️"),
		progressbar.OptionShowCount(),
		progressbar.OptionSetVisibility(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "░",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
	return subBar
}


func getAllConfigPath(configPaths string, skipPaths string) ([]string, error) {
	httpRegex := regexp.MustCompile(`^https?://`)
	var _skipPaths []string
	if skipPaths != "" {
		_skipPaths = strings.Split(skipPaths, ",")
	}

	for i, pattern := range _skipPaths {        
        // 模式也转为绝对路径
        _skipPaths[i], _ = filepath.Abs(pattern) 
		_skipPaths[i] = filepath.ToSlash(_skipPaths[i])
	}

	cfgPaths := strings.Split(configPaths, ",")
	resultPaths := make([]string, 0)

	for _, path := range cfgPaths {

		// 处理HTTP链接
		if httpRegex.MatchString(path) {
			resultPaths = append(resultPaths, path)
			continue
		}

		// 获取绝对路径
		absPath, err := filepath.Abs(path)
		if err != nil {
			log.Fatalln("error to get abs path of: %v", path)
		}

		// 检查文件/目录是否存在
		info, err := os.Stat(absPath)
		if os.IsNotExist(err) {
			log.Fatalln("absPath: %v not exist", absPath)
		}

		// 处理文件
		if !info.IsDir() {
			if isYamlFile(absPath) && !isSkipped(absPath, _skipPaths) {
				resultPaths = append(resultPaths, absPath)
			}
			continue
		}

		// 处理目录
		err = filepath.WalkDir(absPath, func(walkPath string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isYamlFile(walkPath) {
				return err
			}

			if !isSkipped(walkPath, _skipPaths) {
				resultPaths = append(resultPaths, walkPath)
			}
			return nil
		})

		if err != nil {
			log.Fatalln("error walking directory: %w", err)
		}
	}

	return resultPaths, nil
}

func isYamlFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func isSkipped(path string, skipPaths []string) bool {
	for _, pattern := range skipPaths {        
        
        // 通配符匹配
        if match, _ := filepath.Match(pattern, path); match {
            return true
        }

        // 前缀匹配（兼容Windows）
        normalizedPath := filepath.ToSlash(path)
        if strings.HasPrefix(normalizedPath, pattern) {
            return true
        }
    }
    return false
}


func printResults(results []*speedtester.Result) {
	table := tablewriter.NewWriter(os.Stdout)

	table.SetHeader([]string{
		"序号",
		"节点名称",
		"类型",
		"延迟",
		"抖动",
		"丢包率",
		"下载速度",
		"上传速度",
		"自定义网站连通性",
		"自定义网站打开速度",
		"自定义资源下载速度",
	})

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)

	for i, result := range results {
		idStr := fmt.Sprintf("%d.", i+1)

		// 延迟颜色
		latencyStr := result.FormatLatency()
		if result.Latency > 0 {
			if result.Latency < 800*time.Millisecond {
				latencyStr = colorGreen + latencyStr + colorReset
			} else if result.Latency < 1500*time.Millisecond {
				latencyStr = colorYellow + latencyStr + colorReset
			} else {
				latencyStr = colorRed + latencyStr + colorReset
			}
		} else {
			latencyStr = colorRed + latencyStr + colorReset
		}


		jitterStr := result.FormatJitter()
		if result.Jitter > 0 {
			if result.Jitter < 800*time.Millisecond {
				jitterStr = colorGreen + jitterStr + colorReset
			} else if result.Jitter < 1500*time.Millisecond {
				jitterStr = colorYellow + jitterStr + colorReset
			} else {
				jitterStr = colorRed + jitterStr + colorReset
			}
		} else {
			jitterStr = colorRed + jitterStr + colorReset
		}

		// 丢包率颜色
		packetLossStr := result.FormatPacketLoss()
		if result.PacketLoss < 10 {
			packetLossStr = colorGreen + packetLossStr + colorReset
		} else if result.PacketLoss < 20 {
			packetLossStr = colorYellow + packetLossStr + colorReset
		} else {
			packetLossStr = colorRed + packetLossStr + colorReset
		}

		// 下载速度颜色 (以MB/s为单位判断)
		downloadSpeed := result.DownloadSpeed / (1024 * 1024)
		downloadSpeedStr := result.FormatDownloadSpeed()
		if downloadSpeed >= *goodDownloadSpeedThreshold {
			downloadSpeedStr = colorGreen + downloadSpeedStr + colorReset
		} else if downloadSpeed >= *minSpeed + 0.1 {
			downloadSpeedStr = colorYellow + downloadSpeedStr + colorReset
		} else {
			downloadSpeedStr = colorRed + downloadSpeedStr + colorReset
		}

		// 上传速度颜色
		uploadSpeed := result.UploadSpeed / (1024 * 1024)
		uploadSpeedStr := result.FormatUploadSpeed()
		if uploadSpeed >= 0.5 {
			uploadSpeedStr = colorGreen + uploadSpeedStr + colorReset
		} else if uploadSpeed >= 0.2 {
			uploadSpeedStr = colorYellow + uploadSpeedStr + colorReset
		} else {
			uploadSpeedStr = colorRed + uploadSpeedStr + colorReset
		}

		//自定义网站连通性
		extraURLConnectivityStr := result.FormatExtraURLConnectivity()
		if result.ExtraURLConnectivity {
			extraURLConnectivityStr = colorGreen + extraURLConnectivityStr + colorReset
		} else {
			extraURLConnectivityStr = colorRed + extraURLConnectivityStr + colorReset
		}

		
		urlOpenSpeed := result.ExtraURLOpenSpeed / (1024 * 1024)
		extraURLOpenSpeedStr := result.FormatExtraURLOpenSpeed()
		if urlOpenSpeed >= *goodOpenSpeedThreshold {
			extraURLOpenSpeedStr = colorGreen + extraURLOpenSpeedStr + colorReset
		} else if urlOpenSpeed >= *openSpeedThreshold + 0.1 {
			extraURLOpenSpeedStr = colorYellow + extraURLOpenSpeedStr + colorReset
		} else {
			extraURLOpenSpeedStr = colorRed + extraURLOpenSpeedStr + colorReset
		}

		extraDownloadSpeed := result.ExtraDownloadSpeed / (1024 * 1024)
		extraDownloadSpeedStr := result.FormatExtraDownloadSpeed()
		if extraDownloadSpeed >= *goodDownloadSpeedThreshold {
			extraDownloadSpeedStr = colorGreen + extraDownloadSpeedStr + colorReset
		} else if extraDownloadSpeed >= *minSpeed + 0.1 {
			extraDownloadSpeedStr = colorYellow + extraDownloadSpeedStr + colorReset
		} else {
			extraDownloadSpeedStr = colorRed + extraDownloadSpeedStr + colorReset
		}

		row := []string{
			idStr,
			result.ProxyName,
			result.ProxyType,
			latencyStr,
			jitterStr,
			packetLossStr,
			downloadSpeedStr,
			uploadSpeedStr,
			extraURLConnectivityStr,
			extraURLOpenSpeedStr,
			extraDownloadSpeedStr,
		}

		table.Append(row)
	}

	fmt.Println()
	table.Render()
	fmt.Println()
}

func doSaveConfig(results []*speedtester.Result, absPath string) {
	if len(results) == 0 {
		log.Warnln("%s 无任何有效节点信息", absPath)
		return
	}
	proxies := make([]map[string]any, 0)
	for _, result := range results {
		proxies = append(proxies, result.ProxyConfig)
	}

	config := &speedtester.RawConfig{
		Proxies: proxies,
	}
	yamlData, err := yaml.Marshal(config)
	if err != nil {
		log.Fatalln("convert yaml: %s failed: %v", absPath, err)
	}
	err = os.WriteFile(absPath, yamlData, 0o644)
	if err == nil {
		fmt.Printf("\nsave good config file to: %s\n", absPath)
	} else {
		log.Fatalln("save config file: %s failed: %v", absPath, err)
	}
}

func saveConfig(results []*speedtester.Result) {
	if *goodOutputPath != "" {
		absGoodOutputPath, _ := filepath.Abs(*goodOutputPath)
		goodResults := make([]*speedtester.Result, 0)
		for _, result := range results {
			if isProxyGood(result) {
				goodResults = append(goodResults, result)
			}
		}
		doSaveConfig(goodResults, absGoodOutputPath)
	}
	if *outputPath != "" {
		absOutputPath, _ := filepath.Abs(*outputPath)
		doSaveConfig(results, absOutputPath)
	}
}
