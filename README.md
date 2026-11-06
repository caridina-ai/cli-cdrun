# cdrun

在 Windows 上透過命令列啟動、控制獨立的 Codex 背景對話。適合從 PowerShell、腳本或其他程式呼叫。

啟動時指定名稱，之後用名稱或傳回的 PID 傳送提示詞、讀取回答或結束對話。各個 session 獨立執行，可同時操作多個專案。

```text
cdrun -d <working-dir> [-n <name>] [prompt] [-q]     start a session, or prompt the one
                                                     already running; prints its PID
cdrun -n <name> <prompt> [-q]                        inject a prompt into that session
cdrun -n <name> - [-q]                               inject a prompt read from stdin
cdrun -n <name> -s                                   print what that session is showing
cdrun -n <name> -q                                   drain, then end and archive the session
cdrun -p <pid> <prompt> | - | -s | -q                the same, picking the session by PID
cdrun -l [keyword ...]                               list running sessions and their PIDs
cdrun -m <old-dir> <new-dir>                         after a working directory was renamed or
                                                     moved, move what Codex keeps for it
```

## 安裝

需要 Windows、Go 1.26.3 以上，以及已登入的 Codex CLI 或 Codex 桌面版。

```powershell
go install github.com/caridina-ai/cli-cdrun/cmd/cdrun@latest
```

將 Go 的執行檔安裝目錄加入 PATH（預設為 `%USERPROFILE%\go\bin`），即可執行 `cdrun`。更新時重新執行同一條安裝指令。

## 使用

```powershell
# Start a session in the project directory.
$sessionPid = [int](cdrun -d D:\projects\my-project "請先閱讀專案並說明它的用途")

# Send another prompt to the same session.
cdrun -p $sessionPid "請補上使用說明"

# Read answers and session status.
cdrun -p $sessionPid -s
cdrun -p $sessionPid -s --json

# Close the session.
cdrun -p $sessionPid -q
```

啟動時 stdout 只輸出 PID。傳送提示詞成功表示已入列，模型會依序處理；請用 `-s` 查看結果。

### 在 Codex app 和手機上一起看、一起輸入

Codex 的一段對話同一時間只能由一個 app-server 寫入，而 Codex 桌面版的視窗和與它配對的手機，都連到桌面版自己的 app-server。所以只要裝了 Codex 桌面版，cdrun 就把提示詞交給桌面版執行（和 `-q` 一起送出的除外，見下文）：

1. 提示詞排進 Codex 自己的對話佇列（和 `codex queue` 相同）。
2. cdrun 用桌面版的背景喚醒連結（`codex://threads/wake/<thread>`）請它接手。桌面版在背景打開這段對話並執行佇列，不會跳出或切換視窗；桌面版沒在執行時會在背景啟動。
3. cdrun 從自己的唯讀 app-server 追蹤這一輪直到完成；`-s` 顯示狀態與最後的回答。

執行期間（包括等待 sleep 或背景程式），在桌面版或手機上都能用 `-n` 的名稱找到這段對話，即時看到進度，也可以繼續輸入，就像在 app 裡開的對話。核准要求也顯示在 app 裡。

例外是和 `-q` 一起送出的提示詞，例如放在工作排程器裡的 `cdrun -d D:\weather "請更新今日天氣" -q`。這種工作的重點是做完就關掉，所以它由 cdrun 自己的私有 app-server 執行，做完立刻封存；執行期間桌面版和手機會顯示這段對話正由其它程式使用。若桌面版已經開著這段對話（例如先前沒帶 `-q` 的提示詞在那裡執行過），這個提示詞也交給桌面版執行，做完後對話留在桌面版裡（見下文）。沒帶 `-q` 的提示詞一律照上面的方式交給桌面版。

桌面版 60 秒內沒有接手時（例如沒有安裝或沒有登入），cdrun 把提示詞從佇列收回，改由自己的私有 app-server 執行，提示詞不會遺失，也不會重複執行。啟動 session 前設定 `CDRUN_CODEX_APP=0`，則一律由私有 app-server 執行：回合進行中桌面版和手機無法開啟這段對話（會顯示正由其它程式使用），閒置時（`-s` 顯示 `status=released`）才能在 app 接手。

由桌面版執行的回合用的是桌面版的環境，不是呼叫 cdrun 時的環境變數；需要沿用呼叫端環境變數的工作，請使用 `CDRUN_CODEX_APP=0`。`--cancel` 只能中斷 cdrun 自己執行的回合；在桌面版執行的回合請在 app 裡停止。

結束 session 時 cdrun 會封存對話。Codex 不允許封存其它 app-server 正開著的對話，外部也沒有請桌面版放手或代為封存的方法（`codex archive` 同樣會被拒絕）。桌面版在背景執行過的對話會一直開著，直到 app 結束；在視窗打開過的對話則在閒置數小時後才放手，手機正在看時也不放手。所以做完所有已接受的工作後，若桌面版仍開著這段對話，broker 只稍等片刻（約 15 秒，狀態顯示 `archiving`），然後照樣結束，把對話留在桌面版裡，請在 app（PC 或手機）裡封存。桌面版正在執行的回合不受影響。這段期間 broker 不再接受提示詞，也不佔用名稱：同名同目錄的 `-d -n` 會開新的 session，`-n` 也優先指向新的 session。其他封存錯誤最多嘗試三次。

啟動或傳送提示詞時也可以加 `-q`，讓 broker 做完已接受的工作後自動結束並封存。例如：

```powershell
cdrun -d D:\projects\my-project "請完成這項工作" -q
cdrun -p $sessionPid "最後再檢查一次" -q
Get-Content -Raw -Encoding UTF8 prompt.txt | cdrun -n my-project - -q
```

搭配提示詞或 `-d` 的 `-q` 在請求入列後立即返回，exit 0 表示已接受，封存稍後由 broker 執行；啟動或重用仍只在 stdout 輸出 PID。提示詞與關閉要求一起接受，佇列滿載時兩者都不接受。進入關閉流程後不再接受新提示詞。單獨 `-p PID -q` 或 `-n NAME -q` 則等到 session 結束並回報結果，也可以用來等待先前已要求自動關閉的 session（須在 broker 仍執行時呼叫）：封存成功時 stderr 顯示 `exited and archived`；對話仍開在桌面版時顯示 `the Codex app still has its conversation open, so archive it in the app`，兩者都 exit 0；其他封存失敗 exit 1。要讓排程工作做完就封存，請把最後的提示詞和 `-q` 一起送出，不要先送提示詞再另外單獨 `-q`。

以上等待沒有工作期限：即使執行數天，`-q` 也不會中止目前或排隊中的工作。模型等待背景程式的期間，只要回合尚未完成，就繼續等待；模型已結束回合後自行留在背景的程式不算待完成的回合。若要中斷 cdrun 自己執行的回合，可另外使用 `--cancel`；在桌面版執行的回合請在 app 裡停止。

| 指令 | 功能 |
|---|---|
| `-d DIR [-n NAME] [prompt]` | 啟動或重用同名、同目錄的 session，stdout 只印 PID |
| `-n NAME "prompt"` | 傳送提示詞，保留同一段對話的上下文 |
| `-n NAME -` | 從 UTF-8 stdin 讀取多行提示詞 |
| `-n NAME -s` | 顯示最近的對話與狀態 |
| `-n NAME -q` | 結束 session 並回報封存結果 |
| `-p PID <prompt> \| - \| -s \| -q` | 同上，改用 broker PID 指定 |
| `-l [keyword ...]` | 列出符合所有關鍵字的執行中 session |
| `-m OLD NEW` | 目錄改名或搬家之後，把 Codex 為舊路徑記的設定搬到新路徑 |

### 名稱、重複啟動與查詢

```powershell
cdrun -d D:\work\fund -n 00919 "請起來做事"
cdrun -n 00919 "請繼續"
cdrun -l fund 00919
```

`-n` 是完整名稱，不分大小寫，可以全是數字；省略時使用工作目錄的最後一段。`-p` 只接受正整數 PID，不能和 `-n` 同時使用。找不到名稱會提示用 `-l` 查詢；同名多個時拒絕並列出 PID，請改用 `-p`。

排程可以永遠呼叫同一行 `-d DIR -n NAME [prompt]`：同名且同目錄時注入既有 session，沒有 prompt 就直接傳回 PID；沒有同名 session 才新建。同名但目錄不同、或同名不只一個時拒絕，連指定目錄都不會建立。目錄比較不分大小寫，忽略結尾斜線，`/` 與 `\` 相同。省略名稱也套用這些規則。啟動返回時，立刻用 `-n` 就能操作。

`-l` 的欄位依序為 `PID  NAME  STATUS  DIRECTORY`，標題走 stderr，資料走 stdout；中文欄寬也會對齊。關鍵字不分大小寫、部分比對名稱、工作目錄和 thread ID；多個關鍵字全部都要符合。有關鍵字且符合多筆時會提示縮小範圍，沒有符合時 exit 1。

查詢也包含 Codex app／CLI 自己開啟、目前仍持有 writer lock 的對話，不限於 cdrun 啟動的 session。只存在於歷史清單、已沒有程式持有的對話不算執行中。cdrun 會檢查鎖定狀態，排除當機留下的空紀錄，並從 Codex 讀取名稱與目錄。

同一個 Codex app 程序可以承載多段對話。因此首次發現原生對話時，cdrun 會替它建立輕量代理，`-l` 顯示的是各對話自己的代理 PID；用該 PID 或名稱就能注入和查閱。代理不接管原生 writer，提示詞送進 Codex 的 thread queue，由原本的 app／CLI 處理。原生對話釋放 writer 後，代理會自動退出；再次 resume 時重新發現，名稱不變，但代理 PID 可能改變。原生改名也會反映到 `-n` 查詢。

原生對話的 `STATUS=open` 表示仍有程式持有對話，不一定正在生成回答；`-s` 讀取 Codex 已保存的最近回合，可能稍晚於畫面。`-d -n` 也會重用符合名稱與目錄的原生對話。查詢範圍是目前 `CODEX_HOME`，未設定時為 `~/.codex`。代理的程序建立時間、執行檔路徑與端點仍會經過檢查。

broker 的名稱不隨內部 app-server 重啟或重新接回對話而改變。broker 結束後，同一個 `-d -n` 會建立新對話並傳回新 PID；它不會接回已封存的舊對話。

對原生對話執行 `-q` 或 `/exit` 會請求 Codex 封存；若原本的 app／CLI 仍持有 writer 而拒絕，會 exit 1，請在該 app／CLI 結束或封存。原生對話不能把提示詞與 `-q` 合用，cdrun 會在送出提示詞前拒絕；請先傳送提示詞，再在原本的 app／CLI 結束。cdrun 不會終止承載其他對話的共享程序。原生對話的 `--cancel` 請在原本的 app／CLI 操作。

### cdrun 專有功能

| 指令 | 功能 |
|---|---|
| `-n NAME -s --json` | 以 JSON 輸出回答、逐輪狀態與錯誤 |
| `-n NAME --cancel` | 中斷 cdrun 自己執行的當前回合；保留 session 與排隊中的提示詞 |
| `-n NAME /exit` | 等排隊工作完成後結束並封存，注入返回代表已入列 |
| `-d DIR --trust=false` | 停用啟動時的信任覆寫 |
| `--version` | 顯示版本 |
| `--help` | 顯示指令說明 |

上述 session 操作也接受 `-p PID`。`-q` 等待所有已接受的回合完成，沒有強制停止期限，再釋放寫入權與封存；Codex app 仍開著對話時，session 照樣結束，對話留在 app 裡。`/技能名稱` 也是 cdrun 專有功能，見下方。

除 `-q` 可以放在提示詞後，其他選項放在提示詞前，提示詞請整段加引號。未知選項、無效組合與失效 PID 都會報錯。成功一律 exit 0，失敗一律 exit 1。`-l` 只收關鍵字；`-d` 不能搭配 `-p`、`-s`；`-s` 和 `-q` 不能一起使用，`-s` 不能帶 prompt，而且需要 `-p` 或 `-n`。

### 工作目錄與權限

`-d DIR` 預設自動信任指定工作目錄，不需要按 Yes；專案的 `.codex/config.toml` 會正常載入。Codex 可能將目錄的信任狀態保存至使用者設定。

`-d DIR --trust=false` 可停用 cdrun 傳入的信任設定，改由 Codex 自行決定；這個選項不會撤銷既有信任，也不保證 Codex 不記錄新目錄。

模型、沙盒與核准都沿用 Codex 設定（`~/.codex/config.toml`），跟 Codex app 一樣。由桌面版執行的回合，核准要求顯示在桌面版和手機上；由 cdrun 私有 app-server 執行的回合沒有核准視窗，Codex 要求核准的操作一律拒絕。要讓它不停下來問，請在 Codex 設定裡設好，例如：

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
```

目錄改名或搬家之後，Codex 為它記的設定（`config.toml` 裡的 `[projects.'<目錄>']`，例如信任）還留在舊路徑，Codex app 接手的那幾輪就會把新目錄當成陌生的目錄。先搬目錄，再把設定搬過去：

```powershell
ren D:\projects\my-project my-app
cdrun -m D:\projects\my-project D:\projects\my-app
```

新路徑要已經存在、舊路徑要已經不在。新路徑已經有設定時不覆蓋也不合併，報錯 exit 1；舊路徑沒有設定就說一聲，exit 0。

### 技能

提示詞以 `/技能名稱` 開頭時，會明確呼叫該目錄中 Codex 可找到、已啟用的同名技能，後面的參數原樣保留。例如已安裝名為 `review` 的技能時：

```powershell
cdrun -p $sessionPid "/review 請檢查最近的修改"
```

技能名稱支援英文字母、數字、連字號與底線；找不到或有多個同名技能時，該回合會回報錯誤。`/exit` 保留作為關閉指令。

### 從程式讀取結果

`-s --json` 提供以下欄位：

| 欄位 | 意義 |
|---|---|
| `Status`、`Error` | session 狀態與錯誤 |
| `Completed` | 已結束的回合數，包含失敗和中斷 |
| `Queued` | 尚未開始的提示詞數量 |
| `Turns` | 最近的回合，包含 `Sequence`、`Prompt`、`Answer`、`Status`、`Error` |
| `FirstSequence` | 目前保留的第一個回合序號 |

請依回合的 `Sequence`、`Status` 和 `Error` 判斷結果；`Queued=0` 不代表當前回合已完成，注入成功也不代表模型已執行成功。

原生對話的 JSON 另標示 `Native=true`：`Completed` 是此次讀到的最近回合中已完成的數量，`Sequence` 是這一頁的順序，跨次查詢請以 `Turns[].ID` 識別回合。`Queued=-1` 表示無法取得完整佇列數，不代表空佇列。

最多保留最近 16 輪，每輪回答上限 32 KiB，超過時標示 `Truncated=true`。單次提示詞最多 8,000 個 Unicode 字元、stdin 最多 65,536 bytes，佇列最多 32 筆。

## 環境設定

cdrun 依序從 `CDRUN_CODEX_EXE`、PATH、Codex 桌面版安裝目錄尋找 Codex。若需要指定執行檔：

```powershell
$env:CDRUN_CODEX_EXE = 'C:\path\to\codex.exe'
```

回合預設交給 Codex 桌面版執行（見上）。要讓之後啟動的 session 一律由 cdrun 自己的私有 app-server 執行：

```powershell
$env:CDRUN_CODEX_APP = '0'
```

請在同一 Windows 帳號與權限層級啟動和控制 session。一般 session 的狀態與日誌存於 `%LOCALAPPDATA%\cdrun`，管理員 session 存於 `%LOCALAPPDATA%\cdrun-elevated`。每個 session 的事件與 stderr 日誌各上限 8 MiB；關閉後可自行清理診斷目錄。

## 使用範圍

- cdrun 控制背景對話，本身沒有互動終端或桌面視窗；要接手請用 Codex app（見上）。`-n` 設定對話名稱。
- 由 Codex app 執行的回合（預設），用的是 Codex app 的環境，不是 cdrun 啟動時的環境變數；需要時請用 `CDRUN_CODEX_APP=0`。和 `-q` 一起送出、由 cdrun 自己執行的提示詞，則用 cdrun 啟動時的環境。
- PID 屬於 cdrun 代理。cdrun 啟動的 session 關閉時會清理自己的子程序；cdrun 喚醒或在背景啟動的 Codex app 不屬於 session，不會被關閉；原生對話的代理也不會關閉共享的 Codex app／CLI 程序。
- session 結束後請清除保存的 PID，避免日後 Windows 重用同一個 PID。
- 對話各自獨立，但同一目錄中的檔案仍共用；並行修改請使用不同工作目錄。
- 程序失敗時不會自動重送提示詞，以免重複執行有副作用的工作。

開發模型：GPT-6.0（Codex）。
