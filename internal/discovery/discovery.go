package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Server 表示一个可用于建立 Surfshark VPN 连接的配置文件。
type Server struct {
	Country  string
	Name     string
	OvpnPath string
}

// Scan 扫描目录中的 ovpn 文件并按国家归类。
func Scan(dir string) (map[string][]Server, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read ovpn dir %s: %w", dir, err)
	}

	servers := make(map[string][]Server)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ovpn") {
			continue
		}

		country, name := parseFilename(entry.Name())
		if country == "" || name == "" {
			continue
		}

		servers[country] = append(servers[country], Server{
			Country:  country,
			Name:     name,
			OvpnPath: filepath.Join(dir, entry.Name()),
		})
	}

	for country := range servers {
		sort.Slice(servers[country], func(i, j int) bool {
			return servers[country][i].Name < servers[country][j].Name
		})
	}

	return servers, nil
}

// parseFilename 从 Surfshark ovpn 文件名中提取国家和服务器名。
func parseFilename(filename string) (country, server string) {
	if !strings.HasSuffix(filename, ".ovpn") {
		return "", ""
	}

	dotIndex := strings.Index(filename, ".")
	if dotIndex <= 0 {
		return "", ""
	}

	server = filename[:dotIndex]
	dashIndex := strings.Index(server, "-")
	if dashIndex <= 0 {
		return "", ""
	}

	country = strings.ToLower(server[:dashIndex])
	return country, server
}
