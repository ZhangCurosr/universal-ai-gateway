package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ZhangCurosr/universal-ai-gateway/internal/mcp"
)

// 文件中转的客户端子命令（FS-41，T69）：
//
//	gateway files upload <path> [--ttl 2h] [--json]
//	gateway files list [--json]
//	gateway files delete <id>
func cmdFiles(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gateway files upload <path> [--ttl 2h] [--json] | list [--json] | delete <id>")
		return 2
	}
	c, ok := clientEnv(getenv, stderr)
	if !ok {
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("files "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the gateway's JSON response")
	ttl := fs.Duration("ttl", 0, "how long the gateway keeps the file (default: files.default_ttl)")
	pos, err := parseInterspersed(fs, rest)
	if err != nil {
		return 2
	}
	switch sub {
	case "upload":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: gateway files upload <path> [--ttl 2h] [--json]")
			return 2
		}
		info, err := c.UploadFile(ctx, pos[0], *ttl)
		if err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		if *asJSON {
			return printJSON(stdout, stderr, info)
		}
		fmt.Fprintf(stdout, "%s  %d bytes  expires %s\n", info.ID, info.Size, info.ExpiresAt.Local().Format(time.DateTime))
		return 0
	case "list":
		body, err := c.ListFiles(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		if *asJSON {
			fmt.Fprintln(stdout, strings.TrimSpace(string(body)))
			return 0
		}
		var v struct {
			Files []mcp.FileInfo `json:"files"`
			Used  struct {
				Files int   `json:"files"`
				Bytes int64 `json:"bytes"`
			} `json:"used"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			fmt.Fprintln(stderr, "gateway: could not parse the gateway response")
			return 1
		}
		for _, f := range v.Files {
			fmt.Fprintf(stdout, "%s  %10d  %s  %s\n", f.ID, f.Size, f.ExpiresAt.Local().Format(time.DateTime), f.Name)
		}
		fmt.Fprintf(stdout, "%d files, %d bytes\n", v.Used.Files, v.Used.Bytes)
		return 0
	case "delete":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: gateway files delete <id>")
			return 2
		}
		if err := c.DeleteFile(ctx, pos[0]); err != nil {
			fmt.Fprintln(stderr, "gateway:", err)
			return 1
		}
		fmt.Fprintln(stdout, "deleted", pos[0])
		return 0
	}
	fmt.Fprintf(stderr, "gateway: unknown files command %q (upload, list, delete)\n", sub)
	return 2
}

// parseInterspersed 允许位置参数与选项交错（`upload a.pdf --ttl 1h`）。
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func printJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(stderr, "gateway:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

// stringList 是可以重复的字符串选项（--upload-dir）。
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
