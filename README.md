# cdrun

在 Windows 上透過命令列啟動、控制獨立的 Codex 背景對話。適合從 PowerShell、腳本或其他程式呼叫。

啟動時指定名稱，之後用名稱或傳回的 PID 傳送提示詞、讀取回答或結束對話。各個 session 獨立執行，可同時操作多個專案。

```text
cdrun -d <working-dir> [-n <name>] [prompt]          start a session, or prompt the one
                                                     already running; prints its PID
cdrun -n <name> <prompt>                             inject a prompt into that session
cdrun -n <name> -                                    inject a prompt read from stdin
cdrun -n <name> -s                                   print what that session is showing
cdrun -n <name> -q                                   end that session
cdrun -p <pid> <prompt> | - | -s | -q                the same, picking the session by PID
cdrun -l [keyword ...]                               list running sessions and their PIDs
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

### 在 Codex app 裡接手

Codex 的一段對話同一時間只能由一個程式寫入。cdrun 只在有工作時佔用對話，處理完、沒有排隊的提示詞時就放手（`-s` 顯示 `status=released`）。這時可以在 Codex 桌面版，或與它配對的手機上，用 `-n` 的名稱找到這段對話，直接接著對話。這是 Codex 自己保存的對話標題，重新接回或 resume 也會保留。

之後再用 `-p PID` 傳送提示詞時：

- 沒有其他程式開著這段對話：cdrun 拿回對話，自己執行。
- Codex app 正開著這段對話：提示詞排進 Codex 的佇列，由 Codex app 執行，結果顯示在 Codex app 裡；`-s` 中這一輪標示為 `delegated`。

cdrun 正在執行時，Codex app 只能檢視、不能輸入，等它放手即可。

結束 session 時，cdrun 先釋放自己的寫入權，再封存對話，失敗時最多嘗試三次。`-q` 只有確認封存成功才 exit 0；若 Codex app 正持有對話而拒絕封存，或發生其他錯誤，會明確回報失敗並 exit 1。

| 指令 | 功能 |
|---|---|
| `-d DIR [-n NAME] [prompt]` | 啟動或重用同名、同目錄的 session，stdout 只印 PID |
| `-n NAME "prompt"` | 傳送提示詞，保留同一段對話的上下文 |
| `-n NAME -` | 從 UTF-8 stdin 讀取多行提示詞 |
| `-n NAME -s` | 顯示最近的對話與狀態 |
| `-n NAME -q` | 結束 session 並確認封存 |
| `-p PID <prompt> \| - \| -s \| -q` | 同上，改用 broker PID 指定 |
| `-l [keyword ...]` | 列出符合所有關鍵字的執行中 session |

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

對原生對話執行 `-q` 或 `/exit` 會請求 Codex 封存；若原本的 app／CLI 仍持有 writer 而拒絕，會 exit 1，請在該 app／CLI 結束或封存。cdrun 不會終止承載其他對話的共享程序。原生對話的 `--cancel` 請在原本的 app／CLI 操作。

### cdrun 專有功能

| 指令 | 功能 |
|---|---|
| `-n NAME -s --json` | 以 JSON 輸出回答、逐輪狀態與錯誤 |
| `-n NAME --cancel` | 中斷當前回合；保留 session 與排隊中的提示詞 |
| `-n NAME /exit` | 等排隊工作完成後結束並封存，注入返回代表已入列 |
| `-d DIR --trust=false` | 停用啟動時的信任覆寫 |
| `--version` | 顯示版本 |
| `--help` | 顯示指令說明 |

上述 session 操作也接受 `-p PID`。`-q` 先給排隊工作十秒寬限，再要求停止工作，仍會等待釋放寫入權與封存；不會為了結束等待而砍掉正在封存的 broker。整體等待上限八分鐘，逾時回報失敗。`/技能名稱` 也是 cdrun 專有功能，見下方。

選項放在提示詞前，提示詞請整段加引號。未知選項、無效組合與失效 PID 都會報錯。成功一律 exit 0，失敗一律 exit 1。`-l` 只收關鍵字；`-d` 不能搭配 `-p`、`-s`、`-q`；`-s` 和 `-q` 不能一起使用、不能帶 prompt，而且需要 `-p` 或 `-n`。

### 工作目錄與權限

`-d DIR` 預設自動信任指定工作目錄，不需要按 Yes；專案的 `.codex/config.toml` 會正常載入。Codex 可能將目錄的信任狀態保存至使用者設定。

`-d DIR --trust=false` 可停用 cdrun 傳入的信任設定，改由 Codex 自行決定；這個選項不會撤銷既有信任，也不保證 Codex 不記錄新目錄。

模型、沙盒與核准都沿用 Codex 設定（`~/.codex/config.toml`），跟 Codex app 一樣。cdrun 沒有核准視窗，Codex 要求核准的操作一律拒絕；要讓它不停下來問，請在 Codex 設定裡設好，例如：

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
```

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

請在同一 Windows 帳號與權限層級啟動和控制 session。一般 session 的狀態與日誌存於 `%LOCALAPPDATA%\cdrun`，管理員 session 存於 `%LOCALAPPDATA%\cdrun-elevated`。每個 session 的事件與 stderr 日誌各上限 8 MiB；關閉後可自行清理診斷目錄。

## 使用範圍

- cdrun 控制背景對話，本身沒有互動終端或桌面視窗；要接手請用 Codex app（見上）。`-n` 設定對話名稱。
- 由 Codex app 執行的那幾輪，用的是 Codex app 的環境，不是 cdrun 啟動時的環境變數。
- PID 屬於 cdrun 代理。cdrun 啟動的 session 關閉時會清理自己的子程序；原生對話的代理不會關閉共享的 Codex app／CLI 程序。
- session 結束後請清除保存的 PID，避免日後 Windows 重用同一個 PID。
- 對話各自獨立，但同一目錄中的檔案仍共用；並行修改請使用不同工作目錄。
- 程序失敗時不會自動重送提示詞，以免重複執行有副作用的工作。

開發模型：GPT-6.0（Codex）。
