package selfupdate

// UpgradeRunbook 是远程自动升级的完整操作手册。它以 MCP 工具 upgrade.guide
// 的形式暴露，供其它 AI 程序（或人）通过程序本身获取：不需要翻仓库文档，
// 连上任意节点的 MCP 就能读到最新实践与已踩过的坑。
//
// 维护约定：每次升级流程或坑发生变化，同步更新这份文本。
const UpgradeRunbook = `NPA 远程自动升级手册（guardian 守护式，2026-10-29 实测定型）
适用：对任何 NPA 节点做版本升级，无需人工登录目标机器，无需外部分发服务。

== 核心原则 ==
1. 幸存者原则：负责升级的守护进程与被升级的主程序必须是两个独立进程。
   守护先于杀进程启动；发起升级的连接（MCP/ssh.exec）恰好在杀进程瞬间断掉，
   不能依赖它完成后续步骤。
2. 先验证再杀：新二进制必须先落盘、哈希校验通过、并且真的能执行
   （<new> --version 输出期望版本），才允许走杀进程/覆盖。
3. 无 SSH 兜底的节点（如仅靠 NPA MCP 通道的机器）最危险：任何失败=彻底失联。
   这类节点宁可让用户手动操作，也不要做激进重启。
4. 原地同名替换：不改变主程序可执行文件的路径和文件名，否则新进程会重新
   弹网络访问授权（Windows 防火墙）。

== 标准流程（已实测：Windows amd64 ×2、Linux arm64）==
1. 编译：CGO_ENABLED=0 go build -trimpath -ldflags "-s -w
   -X github.com/npa/npa/internal/service.Version=0.4.1-<unix秒>" ./cmd/npa
   版本格式 v0.4.1-<Unix秒>（界面统一加 v 前缀，Version 值本身不带 v）。
2. 传输到目标节点（二选一，均可跨域经上级代理）：
   a. PUT http://<node>:56701/api/file/upload?path=<目标路径>，body 为原始字节
      （注意：是 PUT + raw body，不是 POST、不是 multipart）。
   b. 若目标节点能反向访问某台持有文件的节点：file.Service 的 HTTP 文件面
      会自动走直连/代理链，无需外部分发服务。
3. 哈希校验：sha256sum（Linux）/ certutil -hashfile <path> SHA256（Windows）。
4. 可执行验证：<new> --version 必须输出期望版本且 exit 0。
5. 写守护配置 JSON（放 data_dir 下，如 guardian.json）：
   {"main_path":"<主程序绝对路径>","new_path":"<新二进制>",
    "backup_path":"<备份路径>","data_dir":"<数据目录>",
    "expected_version":"<新版本>","old_pid":<当前PID>}
   PID 获取：netstat -ano | findstr :56701（Windows，LISTENING 行最后一列）/
   netstat -ltnp | grep 56701（Linux）。
6. 分离拉起守护（守护就是新二进制本身加 --guardian-config）：
   - Windows：bat 经 POST /api/file/write 字节精确写入后执行，bat 内容：
     start "" powershell.exe -NoProfile -WindowStyle Hidden -Command
       "Start-Process -FilePath '<new.exe>' -ArgumentList '--guardian-config','<cfg>' -WindowStyle Hidden"
   - Linux/macOS：setsid nohup <new> --guardian-config <cfg> >/dev/null 2>&1 &
7. 轮询 http://<node>:56701/api/info 直到 version 变为新版本（约 5~20s）。
8. 守护行为（自动）：备份旧版→停旧进程→覆盖主程序→拉起→每 5s 巡检
   /api/system 的版本与健康→挂了重启→连续 6 次失败自动回滚备份版→
   稳定后删除临时副本/备份并自删除退出。日志在 <data_dir>/guardian.log。

== 已踩过的坑（务必避开）==
1. 【Windows 弹窗】临时副本不能叫 *.tmp：Start-Process 走 ShellExecute，
   .tmp 无关联会弹「选择应用打开」。命名用 .exe 后缀（如 npa-guardian.exe）。
2. 【ssh.exec 引号】命令里不要带双引号（执行器会多拼引号）；PowerShell
   -Command 不加引号只取首 token；括号+&+重定向的复杂 bat 经 ssh.exec 会崩。
   复杂脚本一律走 /api/file/write 写文件再执行路径——写入内容字节精确。
3. 【上载方法】/api/file/upload 是 PUT + 原始 body；用 POST 或 multipart
   会得到 405 或把表单当文件内容。
4. 【file.write 上限】/api/file/write 只适合 ≤4MiB 文本；大二进制用 upload。
5. 【curl 下载源】外部分发服务（gofilesvr 等）不可依赖：它可能没在跑，
   且 URL 路径因服务而异。优先用节点间文件面。
6. 【certutil 乱码】Windows certutil 输出是 GBK，直接在输出里找期望的
   sha256 十六进制串即可，不要试图解码中文。
7. 【杀进程】Windows 用 taskkill /f /im <进程名>（guardian 已内置）；
   Linux 禁止 pkill -f <路径>——会误杀包含该路径的执行 shell 自己（0.39
   两次失联的根因）。guardian 用 os.FindProcess+Kill 按 PID 杀，安全。
8. 【macOS 无 setsid】用 nohup ... & 代替。
9. 【延迟自删】Windows 运行中的自身删不掉，guardian 起独立 cmd 延迟删除；
   实测偶有残留，失败时手动 del 即可。Unix 无此问题。
10.【发起连接会断】杀进程瞬间 ssh.exec/MCP 调用报 ConnectionResetError
    属预期，不是失败；判断升级是否成功只看 /api/info 的 version。

== 验证清单 ==
- /api/info version == 期望版本
- guardian.log 出现「新版本已稳定服务，清理并退出」
- new_path / backup_path / 守护自身已清理
- 跨节点 ssh.exec 回归（本机→目标节点 echo）
- /api/topology missing=0
`

// Guide 返回升级手册文本。
func Guide() string { return UpgradeRunbook }
