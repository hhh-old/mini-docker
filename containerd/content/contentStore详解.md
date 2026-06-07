# ContentStore 详解

## 整体架构

contentStore 是 mini-docker 项目中**镜像内容（blob）的存储层**，对应 containerd 的 content store 概念。它负责存储镜像的三大核心数据：**manifest（清单）、config（配置）、layer（层文件）**。

```
Image Service
├── metadata.DB       ← 元数据 (BoltDB)
├── content.Store     ← blob 存储 (manifest/config/layer)  ← 就是它
├── Snapshotter       ← 文件系统快照 (OverlayFS)
└── LeaseManager      ← GC 保护机制
```

## 接口定义

`store.go` 定义了 `Store` 接口，提供 7 个方法：

| 方法 | 作用 |
|------|------|
| `Writer` | 创建一个写入器，用于写入 blob 数据 |
| `Reader` | 按 digest 读取 blob 内容 |
| `Delete` | 按 digest 删除 blob |
| `Info` | 查询 blob 的元信息（大小、类型、标签等） |
| `Walk` | 遍历所有 blob 的元信息 |
| `Update` | 更新 blob 的标签 |
| `Exists` | 检查 blob 是否存在 |

`Writer` 接口在 `io.Writer` 基础上增加了 `Commit`（提交并校验 digest）、`Status`（已写字节数）、`Digest`（当前计算的摘要）。

## 实现：fsStore

`filesys.go` 中的 `fsStore` 是 `Store` 的文件系统实现，采用**文件 + BoltDB 双存储**设计：

- **文件系统**：blob 数据以 `sha256:<hash>` 中的 hash 值作为文件名，平铺存放在 `root` 目录下（对齐 containerd 的 `blobs/sha256/` 格式）
- **BoltDB**：blob 的元信息（`Info` 结构体）存放在 `BucketContent` 桶中，key 是 digest

## fsStore 与 contentWriter 的角色

### 场景：拉取一个镜像层

当你执行 `mini-docker pull ubuntu:latest` 时，需要从 registry 下载镜像的每一层。假设下载一个 50MB 的层：

### fsStore 的角色：仓库管理员

`fsStore` 就像一个**仓库管理员**，它知道：
- 货物存放在哪个目录（`root` 路径）
- 货物清单记录在哪里（`db` 数据库）

```go
type fsStore struct {
    root string        // 仓库地址：/var/lib/mini-docker/content/
    db   *metadata.DB  // 货物清单本子
}
```

仓库管理员提供的服务：
- **Writer**：给你一个"临时入库单"，让你把货物写进去
- **Reader**：根据货物编号，把货物取出来给你
- **Delete**：根据货物编号，把货物扔掉
- **Info**：查货物编号对应的元信息（大小、类型、入库时间）

### contentWriter 的角色：临时入库单

当你调用 `fsStore.Writer()` 时，管理员给你一张**临时入库单**（contentWriter）：

```go
type contentWriter struct {
    store     *fsStore   // 管理员是谁
    tmpPath   string     // 临时存放点：/var/lib/.../content/.tmp-1234567-100
    file      *os.File   // 正在写入的临时文件
    hash      hash.Hash  // SHA256 计算器（边写边算）
    written   int64      // 已写入字节数
    mediaType string     // 货物类型
    committed bool       // 是否已正式入库
}
```

### 完整流程图解

```
1. 调用 Writer() 获取 contentWriter
   ┌─────────────────────────────────────────────────────────┐
   │  fsStore.Writer()                                       │
   │  ├── 创建临时文件: .tmp-1234567-100                      │
   │  └── 返回 contentWriter                                 │
   └─────────────────────────────────────────────────────────┘

2. 从 registry 下载数据，边下载边 Write()
   ┌─────────────────────────────────────────────────────────┐
   │  contentWriter.Write(data)                              │
   │  ├── 写入临时文件                                        │
   │  ├── 同时计算 SHA256                                     │
   │  └── 累计 written 字节数                                 │
   └─────────────────────────────────────────────────────────┘

   下载 50MB 数据，Write() 被调用很多次...

3. 下载完成，调用 Commit() 正式入库
   ┌─────────────────────────────────────────────────────────┐
   │  contentWriter.Commit(expectedDigest="sha256:abc123")   │
   │  ├── 关闭临时文件                                        │
   │  ├── 校验: 计算的 digest == 期望的 digest?              │
   │  │   └── 不一致 → 删除临时文件，报错                      │
   │  ├── 重命名: .tmp-1234567-100 → abc123                  │
   │  └── 写入 BoltDB: key="sha256:abc123", value=Info{...}  │
   └─────────────────────────────────────────────────────────┘
```

### 为什么这样设计？

**问题**：如果直接写入最终文件 `abc123`，下载到一半网络断了怎么办？

**后果**：磁盘上有一个不完整的 `abc123` 文件，但数据库里没有记录。下次有人来取这个 digest，会读到损坏的数据。

**解决方案**：临时文件 + Commit 原子操作

1. 先写临时文件 `.tmp-xxx`
2. Commit 时校验 digest
3. 校验通过后 **rename** 为最终文件名（rename 是原子操作）
4. 最后才写入数据库

如果中途失败：
- 临时文件还在，但不会影响正常读取（因为没人知道它的存在）
- 下次 GC 可以清理掉这些孤儿临时文件

### 一句话总结

- **fsStore**：仓库管理员，管理 blob 的存取和元数据
- **contentWriter**：临时入库单，负责把数据安全地写入仓库并校验完整性

## 使用场景

contentStore 被：

1. **Image Service** 持有，用于镜像拉取时存储 manifest/config/layer blob
2. **GC Collector** 通过 `contentDeleter` 适配器调用 `Delete`，清理未被引用的 blob

## 跨平台

`content_other.go` 对非 Linux 平台仅导出一个 `ErrNotSupported` 错误，表明 content store 仅在 Linux 下可用。
