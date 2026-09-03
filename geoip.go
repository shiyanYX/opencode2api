package main

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang"
)

// ======================== 基于 MaxMindDB 的 IP 地理位置查询 ========================

// geoIPCache 缓存 IP → 区域的映射，避免重复查询。
var (
	geoIPCache   = map[string]string{}
	geoIPCacheMu sync.RWMutex
)

// mmdbReader 缓存打开的 MMDB reader。
var (
	mmdbReaderOnce sync.Once
	mmdbReader     *maxminddb.Reader
	mmdbAvail      bool
)

// mmdbSearchPaths 按优先级搜索 MMDB 文件的位置。
var mmdbSearchPaths = []string{
	// 1. 当前工作目录
	filepath.Join(".", "geoip.mmdb"),
	filepath.Join(".", "Country.mmdb"),
	// 2. 程序所在目录
	// 3. mihomo 默认位置
	filepath.Join(os.Getenv("HOME"), ".config", "mihomo", "Country.mmdb"),
	filepath.Join(os.Getenv("HOME"), ".config", "mihomo", "geoip.metadb"),
	filepath.Join(os.Getenv("HOME"), ".config", "mihomo", "geoip.db"),
	// 4. /data 目录（可能有共享的 MMDB）
	"/data/geoip.mmdb",
	"/data/Country.mmdb",
	// 5. 项目目录
	filepath.Join(os.Getenv("HOME"), "project", "opencode2api", "geoip.mmdb"),
	filepath.Join(os.Getenv("HOME"), "project", "opencode2api", "Country.mmdb"),
}

func initMMDB() {
	mmdbReaderOnce.Do(func() {
		for _, p := range mmdbSearchPaths {
			if _, err := os.Stat(p); os.IsNotExist(err) {
				continue
			}
			reader, err := maxminddb.Open(p)
			if err != nil {
				slog.Warn("geoip: failed to open MMDB", "path", p, "error", err)
				continue
			}
			mmdbReader = reader
			mmdbAvail = true
			slog.Info("geoip: MMDB loaded", "path", p)
			return
		}
		slog.Warn("geoip: no MMDB file found in search paths")
		mmdbAvail = false
	})
}

// countryToRegion 将国家代码映射到标准区域标识。
var countryToRegion = map[string]string{
	"US": "us", "JP": "jp", "SG": "sg", "HK": "hk",
	"TW": "tw", "KR": "kr", "AU": "au", "CA": "ca",
	"GB": "eu", "UK": "eu", "DE": "eu", "FR": "eu",
	"NL": "eu", "IT": "eu", "ES": "eu", "SE": "eu",
	"NO": "eu", "FI": "eu", "DK": "eu", "PL": "eu",
	"RU": "ru", "BR": "br", "IN": "in",
}

// lookupGeoRegion 通过 MMDB 查询 IP 的地理位置，返回区域标识。
func lookupGeoRegion(ipStr string) string {
	geoIPCacheMu.RLock()
	if region, ok := geoIPCache[ipStr]; ok {
		geoIPCacheMu.RUnlock()
		return region
	}
	geoIPCacheMu.RUnlock()

	parsedIP := net.ParseIP(ipStr)
	if parsedIP == nil {
		return ""
	}
	if parsedIP.IsLoopback() || parsedIP.IsPrivate() || parsedIP.IsUnspecified() {
		return ""
	}

	initMMDB()
	if !mmdbAvail || mmdbReader == nil {
		return ""
	}

	// MaxMindDB 的 country 记录结构
	var record struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	err := mmdbReader.Lookup(parsedIP, &record)
	if err != nil {
		return ""
	}

	countryCode := strings.ToUpper(record.Country.ISOCode)
	if countryCode == "" {
		return ""
	}

	region := ""
	if r, ok := countryToRegion[countryCode]; ok {
		region = r
	} else {
		region = strings.ToLower(countryCode)
	}

	geoIPCacheMu.Lock()
	geoIPCache[ipStr] = region
	geoIPCacheMu.Unlock()

	slog.Debug("geoip: resolved", "ip", ipStr, "country", countryCode, "region", region)
	return region
}

// inferRegionFromAddress 从节点地址推断区域。
func inferRegionFromAddress(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return ""
		}
		ip = ips[0]
	}

	return lookupGeoRegion(ip.String())
}
