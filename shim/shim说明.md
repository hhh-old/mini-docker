### 为什么exitReady和shutdownDone这个两个通道的关闭方式不一样，是不是shutdownOnce冗余了

这是一个非常深入且专业的问题。答案是：**`shutdownOnce` 绝对不冗余，它是保证 Shim 进程并发安全、防止程序因 Panic 崩溃的关键防线** [5]。

要理解为什么这两个通道的关闭方式不同，以及为什么 `shutdownOnce` 必不可少，我们需要分析它们各自所处的**并发环境**以及 Go 语言中 **Channel 的关闭规则** [5]。

---

### 一、 Go 语言的 Channel 关闭铁律

在 Go 语言中，通道（Channel）的关闭遵循以下一条高压红线：
* **重复关闭一个已经关闭的通道，会直接触发运行时 Panic（崩溃）** [5]：
  `panic: close of closed channel`

因此，如果一个通道可能在多个协程（Goroutine）中被并发地执行 `close`，我们必须使用同步原语（如 `sync.Once`）来确保**有且仅有一个协程执行关闭操作**。

---

### 二、 为什么 `shutdownOnce` 绝对不冗余？

我们来看 `shutdownDone` 处于怎样的并发环境：

#### 1. 多协程并发环境
在代码中，控制 Socket 服务是通过循环不断接受连接的，并且**每一个客户端连接都会在一个独立的、并发的协程中处理**：
```go
func serveControlSocket(...) {
    for {
        conn, err := listener.Accept()
        // ...
        // 每次连接都会新开一个协程！
        go handleShimConn(..., shutdownOnce) 
    }
}
```

#### 2. 并发冲突场景
如果主守护进程（Daemon）因为网络超时、重试机制、或者多个管理线程同时对该容器下发了 `"shutdown"` 命令：
1. 宿主机上会同时产生两个（或多个）Socket 连接。
2. 它们会在不同的协程中同时执行 `handleShimConn`。
3. 它们会同时进入 `case "shutdown"` 分支。

#### 3. 如果没有 `shutdownOnce` 的后果
如果没有 `shutdownOnce`，代码直接写成 `close(shutdownDone)`：
* 协程 A 率先执行 `close(shutdownDone)`，通道正常关闭，Shim 主协程解除阻塞准备退出。
* 几乎同一时间，协程 B 也执行到 `close(shutdownDone)`。
* **灾难发生**：由于通道已被协程 A 关闭，协程 B 再次关闭它，直接导致整个 `mini-docker shim` 进程 **Panic 崩溃** [5]！

* **结论**：`shutdownOnce.Do` 保证了无论有多少个并发连接发送 `"shutdown"` 请求，`close(shutdownDone)` 在整个 Shim 进程的生命周期里 **只会被执行一次**，从而杜绝了因并发关闭通道导致的崩溃。

---

### 三、 为什么 `exitReady` 看起来不需要防范并发关闭？

我们再看 `exitReady` 处于怎样的并发环境：

```go
// 9. 异步等待容器进程退出
go func() {
    exitOnce.Do(func() {
        exitCode := waitForContainerExit(containerPID)
        // ...
        close(exitReady)
        // ...
    })
}()
```

#### 1. 单协程独占环境
在整个 `run` 方法的生命周期中，**异步等待容器进程退出的协程只被启动了一次**（即上面的 `go func() { ... }()`）。
这里只有一个协程，不存在其他协程去竞争关闭 `exitReady` 的情况。

#### 2. 那为什么这里还要用 `exitOnce.Do`？
技术上讲，在**当前的特定代码结构**中，由于该协程仅有一处启动，这里写 `exitOnce` 确实略显保守，即便直接写 `close(exitReady)` 也是安全的。

但保留 `exitOnce` 有以下两个好处（属于**防御性编程**）：
1. **防止代码重构引发 Bug**：如果未来代码重构，在其他地方（比如超时清理协程、外部主动销毁协程）也需要触发“容器退出”的逻辑并尝试关闭 `exitReady`，那么 `exitOnce` 就能自动提供并发安全保护。
2. **对齐语义**：明确地向阅读代码的人表明，`exitReady` 通道的关闭动作在生命周期中是“只发生一次”的。

---

### 总结

| 通道 | 并发环境 | 重复关闭风险 | `sync.Once` 的必要性 |
| :--- | :--- | :--- | :--- |
| **`shutdownDone`** | **高并发**（每个连接一个协程） | **高**（Daemon 重试、并发请求均可触发） | **必须使用**。否则一旦发生并发 `"shutdown"`，程序必定 Panic 崩溃 [5]。 |
| **`exitReady`** | **单协程**（仅有一个专属监听协程） | **极低**（仅由等待进程退出的协程关闭） | **非绝对必要**，但属于优秀的防御性编程实践，防止未来重构引入 Bug。 |