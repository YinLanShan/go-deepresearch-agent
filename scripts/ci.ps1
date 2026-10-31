<#
.SYNOPSIS
    在本地运行与 CI 相同的检查。

.DESCRIPTION
    CI（.github/workflows/ci.yml）的三个 job 分别做：静态检查、测试、竞态检测。
    本脚本把这三步合起来，让"本地通过 = CI 通过"成为可预期的事。

    与 CI 的差异只有一处：CI 用容器内的 PostgreSQL，本地用你机器的 PG。

.PARAMETER Dsn
    PostgreSQL 测试库连接串。不传则相关集成测试自动 skip（只跑纯单元测试）。

.PARAMETER SkipRace
    跳过竞态检测（较慢，约 30-60 秒）。

.EXAMPLE
    # 只跑静态检查与单元测试（不需要数据库）
    .\scripts\ci.ps1

.EXAMPLE
    # 完整检查，含 PostgreSQL 集成测试与竞态检测
    .\scripts\ci.ps1 -Dsn "postgres://postgres:pass@127.0.0.1:5432/dra_test?sslmode=disable"

.EXAMPLE
    # 快速检查（跳过竞态检测）
    .\scripts\ci.ps1 -Dsn $env:DRA_TEST_PG_DSN -SkipRace
#>
[CmdletBinding()]
param(
    [string]$Dsn = "",
    [switch]$SkipRace
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$failed = @()

function Step {
    param([string]$Name, [scriptblock]$Action)
    Write-Host ""
    Write-Host "=== $Name ===" -ForegroundColor Cyan
    & $Action
    if ($LASTEXITCODE -ne 0) {
        Write-Host "FAILED: $Name" -ForegroundColor Red
        $script:failed += $Name
        return
    }
    Write-Host "OK: $Name" -ForegroundColor Green
}

# ─── 1. 静态检查（对应 CI 的 lint job）─────────────────────────────────────

Step 'gofmt' {
    $unformatted = & gofmt -l . 2>&1 | Where-Object { $_ -and $_.Trim() -ne '' }
    if ($unformatted) {
        Write-Host "以下文件未格式化，请运行 gofmt -w ." -ForegroundColor Yellow
        $unformatted | ForEach-Object { Write-Host "  $_" }
        $global:LASTEXITCODE = 1
    } else {
        Write-Host "全部文件格式正确"
        $global:LASTEXITCODE = 0
    }
}

Step 'go vet' { & go vet ./...; $global:LASTEXITCODE = $LASTEXITCODE }

Step 'go mod tidy 一致性' {
    # -diff 只报告差异、不修改文件。
    $diff = & go mod tidy -diff 2>&1
    if ($LASTEXITCODE -ne 0 -or $diff) {
        Write-Host "go.mod / go.sum 与代码不一致，请运行 go mod tidy" -ForegroundColor Yellow
        $diff | ForEach-Object { Write-Host "  $_" }
        $global:LASTEXITCODE = 1
    } else {
        Write-Host "go.mod 已整理"
        $global:LASTEXITCODE = 0
    }
}

Step '前端 JS 语法' { & node scripts/check-web-js.mjs; $global:LASTEXITCODE = $LASTEXITCODE }

# ─── 2. 构建与测试（对应 CI 的 test job）───────────────────────────────────

Step 'go build' { & go build ./...; $global:LASTEXITCODE = $LASTEXITCODE }

if ($Dsn) {
    Write-Host "使用 PostgreSQL 测试库（集成测试会真实执行）" -ForegroundColor Cyan
    $env:DRA_TEST_PG_DSN = $Dsn
} else {
    Write-Host "未提供 -Dsn，PostgreSQL 集成测试将自动 skip" -ForegroundColor Yellow
    Remove-Item Env:\DRA_TEST_PG_DSN -ErrorAction SilentlyContinue
}

Step 'go test ./...' {
    & go test -count=1 -timeout 300s ./...
    $global:LASTEXITCODE = $LASTEXITCODE
}

# ─── 3. 竞态检测（对应 CI 的 race job）────────────────────────────────────

if (-not $SkipRace) {
    Step 'go test -race（并发敏感包）' {
        & go test -count=1 -race -timeout 300s `
            ./internal/storage/... `
            ./internal/researchstate/... `
            ./internal/scheduler/... `
            ./internal/workpool/... `
            ./internal/resultagg/... `
            ./internal/supervisor/...
        $global:LASTEXITCODE = $LASTEXITCODE
    }
} else {
    Write-Host ""
    Write-Host "跳过竞态检测（-SkipRace）" -ForegroundColor Yellow
}

# ─── 汇总 ─────────────────────────────────────────────────────────────────

Write-Host ""
if ($failed.Count -gt 0) {
    Write-Host "=== 有检查未通过 ===" -ForegroundColor Red
    $failed | ForEach-Object { Write-Host "  ✗ $_" -ForegroundColor Red }
    exit 1
}
Write-Host "=== 全部检查通过 ===" -ForegroundColor Green
exit 0
