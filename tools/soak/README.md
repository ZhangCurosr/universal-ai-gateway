# T61 本机稳定性基线

Python 3 标准库驱动，网关用正常的零 CGO 二进制。所有地址均为回环地址，所有上游与凭据均为合成数据，不连接真实服务。

```bash
CGO_ENABLED=0 go build -o /tmp/gateway-soak ./cmd/gateway
python3 tools/soak/soak.py --binary /tmp/gateway-soak --output /tmp/gateway-soak-results --seconds 86400 --source-commit "$(git rev-parse HEAD)"
```

输出目录必须不存在；所有父目录须可信。脚本在 Linux 上通过 `/proc/<pid>/status` 采集网关 RSS。Windows 未实测，不能直接使用这个内存采集方式。

默认 4 个固定 worker、合计目标 2 请求/秒，覆盖缓冲透传、SSE、Chat → Anthropic 转译、REST。每 5 分钟在两种调度策略之间热重载。每个网络操作最多 5 秒；响应上限 1 MiB。

每 10 秒写入 `samples.csv` 和 `latest.json`：RSS、goroutine、累计请求与错误数、计费队列、审计丢弃、数据目录大小。`status.json` 表示 running / passed / failed，`summary.json` 只在进程退出并核对落库记录后生成。延迟用固定桶计数，分位数只能取所在桶的上界，不能当作精确采样值。

到期自动退出并优雅关闭网关；Ctrl+C / SIGTERM 会标为未完成。任一非预期响应、资源采集失败、RSS 超过 768 MiB、goroutine 超过 512、数据库文件超过 1 GiB 都会提前结束并标为失败。短测通过不能替代 24 小时结果。

进程应在稳定供电、不休眠的机器上执行。保持输出目录供复查；内含合成主密钥和测试数据库，权限按私有文件设置，程序输出不显示 Token。资源数据与延迟仅代表这个负载和这台机器，不是生产吞吐保证。
