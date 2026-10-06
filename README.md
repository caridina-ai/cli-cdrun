# cdrun

在 Windows 上透過命令列啟動、控制獨立的 Codex 背景對話。適合從 PowerShell、腳本或其他程式呼叫。

每次啟動傳回一個 PID，之後用同一個 PID 傳送提示詞、讀取回答或結束對話。各個 session 獨立執行，可同時操作多個專案。

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

Codex 的一段對話同一時間只能由一個程式寫入。cdrun 只在有工作時佔用對話，處理完、沒有排隊的提示詞時就放手（`-s` 顯示 `status=released`）。這時可以在 Codex 桌面版，或與它配對的手機上，用 `-r` 的名稱找到這段對話，直接接著對話。

之後再用 `-p PID` 傳送提示詞時：

- 沒有其他程式開著這段對話：cdrun 拿回對話，自己執行。
- Codex app 正開著這段對話：提示詞排進 Codex 的佇列，由 Codex app 執行，結果顯示在 Codex app 裡；`-s` 中這一輪標示為 `delegated`。

cdrun 正在執行時，Codex app 只能檢視、不能輸入，等它放手即可。

結束 session 時，cdrun 會封存這段對話，跟在 Codex app 裡封存一樣。Codex app 當時正開著這段對話的話，Codex 不允許封存，對話就留在 app 裡。

| 指令 | 功能 |
|---|---|
| `-d DIR [-r NAME] [prompt]` | 啟動新 session；目錄不存在時自動建立，`-r` 指定對話名稱 |
| `-p PID "prompt"` | 傳送提示詞，保留同一段對話的上下文 |
| `-p PID -` | 從 UTF-8 stdin 讀取多行提示詞 |
| `-p PID -s` | 顯示最近的對話與狀態 |
| `-p PID -s --json` | 以 JSON 輸出回答、逐輪狀態與錯誤 |
| `-p PID --cancel` | 中斷當前回合；保留 session 與排隊中的提示詞 |
| `-p PID /exit` | 等排隊工作完成後結束 session，並封存這段對話 |
| `-p PID -q` | 請求結束並封存對話，寬限十秒後強制關閉仍在執行的 session |
| `--version` | 顯示版本 |
| `--help` | 顯示指令說明 |

選項放在提示詞前，提示詞請整段加引號。未知選項、無效組合與失效 PID 都會報錯。

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

最多保留最近 16 輪，每輪回答上限 32 KiB，超過時標示 `Truncated=true`。單次提示詞最多 8,000 個 Unicode 字元、stdin 最多 65,536 bytes，佇列最多 32 筆。

## 環境設定

cdrun 依序從 `CDRUN_CODEX_EXE`、PATH、Codex 桌面版安裝目錄尋找 Codex。若需要指定執行檔：

```powershell
$env:CDRUN_CODEX_EXE = 'C:\path\to\codex.exe'
```

請在同一 Windows 帳號與權限層級啟動和控制 session。一般 session 的狀態與日誌存於 `%LOCALAPPDATA%\cdrun`，管理員 session 存於 `%LOCALAPPDATA%\cdrun-elevated`。每個 session 的事件與 stderr 日誌各上限 8 MiB；關閉後可自行清理診斷目錄。

## 使用範圍

- cdrun 控制背景對話，本身沒有互動終端或桌面視窗；要接手請用 Codex app（見上）。`-r` 設定對話名稱。
- 由 Codex app 執行的那幾輪，用的是 Codex app 的環境，不是 cdrun 啟動時的環境變數。
- PID 屬於 cdrun 代理。關閉 session 會清理它自己的子程序，不會關閉其他 Codex 對話。
- session 結束後請清除保存的 PID，避免日後 Windows 重用同一個 PID。
- 對話各自獨立，但同一目錄中的檔案仍共用；並行修改請使用不同工作目錄。
- 程序失敗時不會自動重送提示詞，以免重複執行有副作用的工作。

開發模型：GPT-6.0（Codex）。
