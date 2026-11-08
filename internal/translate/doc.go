// Package translate 实现 IR 与各协议之间的转换（M2，spec/10，任务卡 T20 ~ T27）。
//
// 每个方向分开实现并独立测试（spec/10 §1 第 4 条）：入站解析（Lower，协议 → IR）与
// 出站合成（Synthesize，IR → 协议）。本包不得导入具体适配器包（spec/10 §0），协议名见 protocols.go。
package translate
