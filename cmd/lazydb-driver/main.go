// lazydb-driver：驱动代理进程（ADR-0002）。一种数据源一个二进制，
// 与主程序经 stdio 行分隔 JSON 通信；连接配置在 open 请求里。
package main

import "github.com/lazygophers/lazydb/internal/driveragent"

func main() { driveragent.Main() }
