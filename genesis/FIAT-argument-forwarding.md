# 把 -- 之後的參數原封不動傳給 Codex

目前 cdrun 啟動 Codex 時，只會帶 `app-server`、私有 stdio 傳輸與工作目錄的信任覆寫。有些 session 需要額外的 Codex 參數，例如透過 `-c model=...` 指定模型、`-c model_reasoning_effort=...` 設定推理程度，或用 `--enable` 啟用功能，目前都無法透過 cdrun 帶進去。

讓 `--` 之後的參數，依原順序與參數邊界傳給私有 `codex app-server`。

```powershell
cdrun -d D:\work\project -n mybot "請檢查專案" -- -c model=gpt-6.1-sol
cdrun -d D:\work\project "請完成這項工作" -n batch -q -- -c 'model_reasoning_effort="high"'
```

- `--` 之前是 cdrun 自己的選項與提示詞，選項和提示詞可以交錯。
- 第一個獨立的 `--` 之後，每個參數都不重新斷詞、不改寫，照順序傳給 Codex，包括含空白、引號、空字串與後續的 `--`。
- 只在 `-d` 建立新 session 時有效。重用同名 session、`-p`／`-n` 注入、`-s`、獨立的 `-q`、`-l`、`-m` 等不啟動新 session 的用法，若帶非空的轉送參數，要在產生副作用之前報錯，不可默默忽略。空的 `--` 視同未帶參數。
- 舊的 `--`「之後都當提示詞」語意移除；提示詞一律放在 `--` 之前。usage、README 與測試一起更新，版本升為 v0.5.0。
- 參數是 app-server 的參數，不是互動式 Codex CLI 的參數；模型使用 `-c model=...`，不把 `--model` 翻譯成另一種形式。未知選項由 Codex 判定並回報啟動失敗。
- `--stdio`、`--listen` 與 cdrun 擁有的私有傳輸衝突時直接報錯；自動信任的 config override 放在轉送參數之後，以 cdrun 的信任值為準，可用 `--trust=false` 停用。將規則寫進 README。
- 有轉送參數時，回合由該 session 的私有 app-server 執行，避免桌面版使用不同設定而使參數失效；每次重啟私有 app-server 都套用同一組參數。不重啟、不重設 Codex app。未帶參數時維持桌面版接手的原有行為。
- 保留 PID-only stdout、每個 session 的私人 broker 與 Job Object、完整排空已接受工作的 `-q` 行為，不更動使用者的全域設定檔來實作參數轉送。
