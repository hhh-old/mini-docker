
### 1. `unix.Mount` 

**函数签名：**
`unix.Mount(source string, target string, fstype string, flags uintptr, data string) error`

**参数含义：**
*   **`source` (源)**：你要挂载的东西在哪。它可以是一个真实的磁盘设备（如 `/dev/sda1`），可以是一个目录（用于 bind mount），对于一些虚拟文件系统（如 proc, overlayfs），这里填个代号就行（比如填 `"overlay"` 甚至留空 `""`）。
*   **`target` (目标)**：你要把它挂载到哪个目录下（挂载点）。比如 `/mnt/data` 或容器的 `rootFSPath`。
*   **`fstype` (文件系统类型)**：告诉内核怎么去解析源数据。常见的有：`"ext4"`, `"overlay"`, `"proc"`, `"tmpfs"`, `"bind"` 等。如果不填（留空），通常意味着这不是普通的磁盘挂载。
*   **`flags` (标志位)**：控制挂载的行为，使用 `uintptr` 类型的位掩码。比如：
    *   `unix.MS_BIND`：将一个目录绑定到另一个目录。
    *   `unix.MS_RDONLY`：只读挂载。
    *   `unix.MS_PRIVATE`：私有挂载，挂载事件不跟宿主机共享。
    *   `unix.MS_REC`：递归应用这些标志位到所有子目录。
*   **`data` (特殊选项)**：针对特定文件系统的配置字符串。
    *   比如 OverlayFS 的：`"lowerdir=/a,upperdir=/b,workdir=/c"`
    *   比如 TmpFS（内存盘）的：`"size=64m"`

**执行效果：**
让内核在虚拟文件系统（VFS）树中建立映射关系。执行成功后，当你访问 `target` 目录时，内核实际上会把你重定向到 `source` 指定的数据、目录或虚拟设备上。

---

### 2. `unix.PivotRoot` (对应内核调用：`pivot_root(2)`)

这个是容器技术的灵魂，也是最难理解的系统调用之一。

**函数签名：**
`unix.PivotRoot(newroot string, putold string) error`

**参数含义：**
*   **`newroot` (新根)**：你希望未来作为整个系统 `/`（根目录）的那个目录路径。在容器中，这就是你准备好的那个包含了 busybox 的 `rootFSPath`。**（注意：内核要求这个目录本身必须是一个挂载点，这也是为什么前面代码里要对自己做一次 bind mount 的原因）**。
*   **`putold` (旧根安放地)**：你当前的系统根目录（也就是宿主机的 `/`）被替换掉后，不能直接凭空消失，内核需要你提供一个目录，用来临时存放原来的宿主机根目录。通常是 `newroot` 下面的一个叫 `.pivot_root` 的临时空文件夹。

**执行效果（偷天换日）：**
极其暴力且彻底。内核会把当前进程（及其子进程）所在的整个文件系统树“连根拔起”。
它将宿主机的 `/` 移动到了 `putold` 目录里，然后把 `newroot` 提升为了这棵树新的 `/`。
自此之后，当前进程的世界观彻底改变，它眼中的 `/` 已经是容器的目录了，而真实的宿主机环境被降级到了 `/.pivot_root` 目录下（随后代码里会把这个目录卸载并删除，彻底切断退路）。

