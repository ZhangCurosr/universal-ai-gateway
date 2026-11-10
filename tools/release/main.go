// Command release 自动化构建多平台发布二进制并生成校验和（spec/12 §7 REL-01 ~ REL-08）。
//
//	go run ./tools/release                      构建全部平台
//	go run ./tools/release -allow-dirty         允许工作区不干净（本地试验）
//	go run ./tools/release -platforms linux/amd64,windows/amd64
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

var defaultPlatforms = []string{
	"linux/amd64",
	"linux/arm64",
	"windows/amd64",
	"darwin/arm64",
}

type platform struct {
	goos   string
	goarch string
}

func parsePlatforms(s string) ([]platform, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("empty platforms list")
	}
	parts := strings.Split(s, ",")
	res := make([]platform, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		items := strings.Split(p, "/")
		if len(items) != 2 || items[0] == "" || items[1] == "" {
			return nil, fmt.Errorf("invalid platform format %q (expected GOOS/GOARCH)", p)
		}
		res = append(res, platform{goos: items[0], goarch: items[1]})
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no valid platforms parsed")
	}
	return res, nil
}

// parseVersion 根据 git describe 输出解析语义化版本（REL-02）。
func parseVersion(describeOut string) string {
	raw := strings.TrimSpace(describeOut)
	if strings.HasPrefix(raw, "v") {
		return raw
	}
	// 无 tag 时为 v0.0.0-<hash>[-dirty]
	if strings.HasSuffix(raw, "-dirty") {
		hash := strings.TrimSuffix(raw, "-dirty")
		return fmt.Sprintf("v0.0.0-%s-dirty", hash)
	}
	return fmt.Sprintf("v0.0.0-%s", raw)
}

// artifactFilename 生成产物文件名（REL-04）。
func artifactFilename(version, goos, goarch string) string {
	name := fmt.Sprintf("universal-ai-gateway_%s_%s_%s", version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// formatChecksumLine 生成符合 sha256sum 格式的行（小写 64 字节十六进制 + 两个空格 + 文件名）。
func formatChecksumLine(sumHex, filename string) string {
	return fmt.Sprintf("%s  %s\n", strings.ToLower(sumHex), filename)
}

func main() {
	allowDirty := flag.Bool("allow-dirty", false, "allow dirty working tree for local testing")
	platformsFlag := flag.String("platforms", strings.Join(defaultPlatforms, ","), "comma-separated list of GOOS/GOARCH platforms")
	flag.Parse()

	targetPlatforms, err := parsePlatforms(*platformsFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: parse platforms: %v\n", err)
		os.Exit(1)
	}

	// 1. REL-05: 检查工作区是否干净
	if !*allowDirty {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		statusOut, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: check git status: %v\n", err)
			os.Exit(1)
		}
		if len(bytes.TrimSpace(statusOut)) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: working tree is dirty; commit or stash changes, or use -allow-dirty for local experiments\n")
			os.Exit(1)
		}
	}

	// 2. REL-05 / REL-08: 运行 tools/check（最长 15 分钟）
	fmt.Println("==> Running tools/check...")
	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 15*time.Minute)
	checkCmd := exec.CommandContext(checkCtx, "go", "run", "./tools/check")
	checkCmd.Stdout = os.Stdout
	checkCmd.Stderr = os.Stderr
	if err := checkCmd.Run(); err != nil {
		cancelCheck()
		fmt.Fprintf(os.Stderr, "FAIL: pre-build check failed: %v\n", err)
		os.Exit(1)
	}
	cancelCheck()

	// 3. REL-02: 获取版本号
	descCtx, cancelDesc := context.WithTimeout(context.Background(), 30*time.Second)
	descOut, err := exec.CommandContext(descCtx, "git", "describe", "--tags", "--always", "--dirty").Output()
	cancelDesc()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: git describe: %v\n", err)
		os.Exit(1)
	}
	version := parseVersion(string(descOut))
	fmt.Printf("==> Target release version: %s\n", version)

	// 4. 清理并准备 dist 目录
	distDir := "dist"
	if err := os.RemoveAll(distDir); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: clean dist directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: create dist directory: %v\n", err)
		os.Exit(1)
	}

	type checksumEntry struct {
		filename string
		sumHex   string
	}
	var entries []checksumEntry

	// 5. REL-02 ~ REL-04 / REL-08: 逐个平台构建
	ldflags := fmt.Sprintf("-s -w -X main.version=%s", version)
	for _, p := range targetPlatforms {
		filename := artifactFilename(version, p.goos, p.goarch)
		outPath := filepath.Join(distDir, filename)
		fmt.Printf("==> Building %s/%s -> %s...\n", p.goos, p.goarch, outPath)

		buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
		buildCmd := exec.CommandContext(buildCtx, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", outPath, "./cmd/gateway")
		buildCmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+p.goos,
			"GOARCH="+p.goarch,
		)
		buildCmd.Stdout = os.Stdout
		buildCmd.Stderr = os.Stderr
		err := buildCmd.Run()
		cancelBuild()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: build failed for platform %s/%s: %v\n", p.goos, p.goarch, err)
			os.Exit(1)
		}

		// 计算 sha256
		f, err := os.Open(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: open artifact for checksum: %v\n", err)
			os.Exit(1)
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "FAIL: compute checksum: %v\n", err)
			os.Exit(1)
		}
		f.Close()
		sumHex := fmt.Sprintf("%x", h.Sum(nil))
		entries = append(entries, checksumEntry{filename: filename, sumHex: sumHex})
	}

	// 6. REL-04: 生成 SHA256SUMS，按文件名排序
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].filename < entries[j].filename
	})
	var sumsBuf bytes.Buffer
	for _, e := range entries {
		sumsBuf.WriteString(formatChecksumLine(e.sumHex, e.filename))
	}
	sumsPath := filepath.Join(distDir, "SHA256SUMS")
	if err := os.WriteFile(sumsPath, sumsBuf.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: write SHA256SUMS: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("==> Generated %s\n", sumsPath)

	// 7. REL-07: 对与当前机器同平台的产物执行 `<产物> version` 自检
	hostGOOS, hostGOARCH := runtime.GOOS, runtime.GOARCH
	var hostArtifact string
	for _, p := range targetPlatforms {
		if p.goos == hostGOOS && p.goarch == hostGOARCH {
			hostArtifact = filepath.Join(distDir, artifactFilename(version, hostGOOS, hostGOARCH))
			break
		}
	}
	if hostArtifact == "" {
		fmt.Printf("note: current host %s/%s was not in build targets; skipping version self-test\n", hostGOOS, hostGOARCH)
	} else {
		fmt.Printf("==> Verifying host artifact version: %s version...\n", hostArtifact)
		verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 10*time.Second)
		verOut, err := exec.CommandContext(verifyCtx, hostArtifact, "version").Output()
		cancelVerify()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: host artifact version check failed: %v\n", err)
			os.Exit(1)
		}
		gotVersion := strings.TrimSpace(string(verOut))
		if gotVersion != version {
			fmt.Fprintf(os.Stderr, "FAIL: version mismatch: got %q, want %q\n", gotVersion, version)
			os.Exit(1)
		}
		fmt.Printf("==> Verified version %q successfully\n", gotVersion)
	}

	fmt.Println("RELEASE BUILD COMPLETED SUCCESSFULLY")
}
