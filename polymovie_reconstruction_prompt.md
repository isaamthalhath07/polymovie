# Polymovie — Complete Reconstruction Prompt

> **Purpose**: This document is a self-contained, hyper-detailed prompt that instructs an LLM to rebuild the entire Polymovie project from absolute zero. Every architectural decision, data flow, constant, function signature, and edge-case handler is specified.

---

## 1. PROJECT IDENTITY

**Name**: `Polymovie`
**Language**: Go (module name: `polymovie`, Go 1.26+)
**Platform**: Windows (paths use `\`, Chrome profile at `%LOCALAPPDATA%`)
**Purpose**: An end-to-end automation pipeline that:

1. Launches a real Chrome browser using the user's existing login session
2. Navigates to an Amazon Prime Video show page
3. Selects a specific season and episode
4. Clicks play and enables closed captions
5. Intercepts `.ttml2` subtitle files from the network layer via Chrome DevTools Protocol
6. Falls back to extracting subtitle URLs from Amazon's `GetPlaybackResources` API response
7. Parses the TTML2 XML into timestamped plain text with speaker labels
8. Sends the full subtitle text to the Google Gemini API with a forensic analysis prompt
9. Saves both the raw subtitles and the AI analysis to local files

**Files to create**:
- `go.mod` — module definition
- `main.go` — the entire application (~1140 lines)
- `dump.go` — standalone DOM debugging utility (separate `main`, used only for debugging)
- `prime_subtitle_grabber.js` — an alternative Node.js/Puppeteer implementation (standalone)

---

## 2. DEPENDENCIES (go.mod)

```
module polymovie
go 1.26

require (
    github.com/chromedp/cdproto   (latest)
    github.com/chromedp/chromedp  (latest)
)
```

The only external dependencies are `chromedp` (Go bindings for Chrome DevTools Protocol) and `cdproto` (the raw CDP protocol types). All other functionality uses the Go standard library.

---

## 3. HIGH-LEVEL ARCHITECTURE (main.go)

The application is a single `main.go` file organized into **7 clearly-delineated sections**, each separated by a full-width comment banner (`// ====...`). The sections are:

```
1. CONFIGURATION         — all constants (URLs, seasons, API keys, timeouts, the Gemini prompt)
2. main()                — the orchestrator: launch → navigate → capture → parse → analyze → save
3. CHROME LAUNCHER       — clones the user's Chrome profile to a temp dir, launches headed Chrome
4. EPISODE NAVIGATION    — navigateToEpisode, selectSeason, clickEpisode, clickPlay, enableSubtitles
5. NETWORK MONITOR       — CDP event listener for .ttml2 responses and GetPlaybackResources API calls
6. RESPONSE & DOWNLOAD   — selecting the best captured response, HTTP fallback download, API URL extraction
7. TTML2 PARSER          — XML stream parser that extracts timestamped text from TTML2
8. GEMINI API            — direct HTTP POST to Gemini REST API with retry logic
9. UTILITIES             — resolveAPIKey, mustRun, writeFile, truncate, copyFile
```

---

## 4. SECTION-BY-SECTION SPECIFICATION

### 4.1 CONFIGURATION (Constants Block)

Define a single `const` block with these exact constants:

| Constant | Type | Value | Purpose |
|---|---|---|---|
| `showURL` | string | Amazon Prime Video detail URL | Target show page |
| `targetSeason` | int | 1 | Which season to select |
| `targetEpisode` | int | 1 | Which episode to click |
| `chromeUserDataDir` | string | `C:\Users\isaam\AppData\Local\Google\Chrome\User Data` | Real Chrome profile root |
| `chromeProfile` | string | `"Default"` | Chrome profile folder name |
| `subtitleExt` | string | `".ttml2"` | File extension to match in network URLs |
| `maxWaitTime` | duration | `90s` | Maximum time to wait for subtitle network responses |
| `settleTime` | duration | `8s` | How long to wait after last subtitle response before declaring "done" |
| `hardcodedAPIKey` | string | Gemini API key | For the Gemini REST API |
| `geminiModel` | string | `"gemini-2.5-flash"` | Which Gemini model to call |
| `apiTimeout` | duration | `5min` | HTTP client timeout for Gemini calls |
| `gptPrompt` | string | *(see below)* | The full forensic analysis prompt |
| `subtitleFile` | string | `"subtitle_output.txt"` | Where to save extracted subtitles |
| `analysisFile` | string | `"analysis_output.txt"` | Where to save Gemini's analysis |
| `preferSDH` | bool | `true` | Prefer SDH/CC tracks (have speaker labels like `[Homelander]`) |
| `minRuntimeSecs` | int | `300` | Minimum content runtime to accept (filters out trailers) |

**The Gemini Prompt** (`gptPrompt`): This is a massive multi-paragraph forensic analysis prompt that instructs the LLM to act as a "forensic script analyst" for The Boys. It tracks 11 named characters (Homelander, Billy Butcher, Starlight, Hughie, Soldier Boy, Ashley, Mother's Milk, The Deep, Sister Sage, Ryan Butcher, Kimiko Miyashiro) and must:

- Distinguish 9 categories of death status (confirmed, implied, fake-out, fatal injury, sacrifice, survival with risk, missing, hallucination/clone, symbolic death)
- Track status chronologically across episodes
- Quote exact evidence from the script text
- Pay attention to stage directions, sound cues, dialogue keywords ("dead", "die", "killed", "no pulse", etc.)
- Use a per-character checklist (first appearance, last alive moment, death confirmation, cause, witnesses, on/off screen, confidence level, contradictions)
- Output a structured forensic report with 5 sections: Executive Summary, Character Death Ledger, Chronological Death Tracker, Ambiguities, Final Verdict

The prompt ends with `--- SUBTITLE TEXT BEGINS ---\n` and the subtitle text is appended directly after it.

### 4.2 DATA STRUCTURES

```go
type capturedSub struct {
    requestID  network.RequestID   // CDP request ID for body retrieval
    url        string              // The full URL of the .ttml2 file
    size       float64             // EncodedDataLength from LoadingFinished event
}

type monitorState struct {
    mu        sync.Mutex
    responses map[network.RequestID]*capturedSub   // captured .ttml2 responses
    apiReqIDs []network.RequestID                   // GetPlaybackResources request IDs
    done      chan struct{}                          // signals when monitoring is complete
}
```

### 4.3 main() — The Orchestrator

The `main()` function executes this exact sequence:

1. `start := time.Now()` — record start time for elapsed calculation
2. Configure logger: `log.SetFlags(log.Ltime | log.Lmicroseconds)`
3. Print banner: `=== Polymovie Subtitle Extractor ===`
4. Resolve API key (hardcoded → env fallback)
5. Validate `showURL` is not empty
6. **Launch Chrome** → get context + cleanup defer
7. **Enable CDP network monitoring**: `network.Enable()`
8. **Start network listener** → returns `*monitorState`
9. **Navigate to episode** (navigate → select season → click episode → click play)
10. **Enable subtitles/CC** in the player
11. **Start settle timer** (begins the countdown for network quiescence)
12. **Block on `<-mon.done`** (wait for network monitoring to complete)
13. **Path 1**: If direct `.ttml2` responses were captured, download the largest one via `network.GetResponseBody`
14. **Path 2** (fallback): Extract subtitle URLs from the `GetPlaybackResources` API response body, then download each URL via plain HTTP GET, concatenating all results
15. **Parse TTML2** XML into timestamped plain text
16. **Save subtitles** to `subtitle_output.txt`
17. **Call Gemini** with prompt + subtitle text
18. **Save analysis** to `analysis_output.txt`
19. Print elapsed time and file sizes

### 4.4 CHROME LAUNCHER — `launchChrome()`

**Signature**: `func launchChrome() (context.Context, func())`

**Critical design decision**: Amazon blocks Chrome automation detection and won't serve subtitles in headless mode. The solution:

1. **Create a temp directory** via `os.MkdirTemp("", "polymovie-profile-*")`
2. **Clone critical session files** from the user's real Chrome profile into the temp dir. This preserves the Amazon login session without locking the user's real Chrome. Files copied:
   - Root profile files: `Cookies`, `Cookies-journal`, `Login Data`, `Login Data-journal`, `Web Data`, `Web Data-journal`, `Preferences`, `Secure Preferences`, `Network Action Predictor`, `TransportSecurity`
   - `Network/` subdirectory: `Cookies`, `Cookies-journal`, `TransportSecurity` (newer Chrome stores cookies here)
   - `Local State` from the parent UserData dir
3. **Configure Chrome flags** via `chromedp.ExecAllocatorOptions`:
   - `UserDataDir` → temp dir
   - `profile-directory` → "Default"
   - `disable-blink-features=AutomationControlled` — **critical**: hides webdriver fingerprint
   - `disable-extensions`, `disable-default-apps`, `disable-translate`, `disable-sync`, `disable-background-networking`, `disable-popup-blocking`, `metrics-recording-only`, `no-first-run`
   - `WindowSize(1920, 1080)`
   - Headless is **DISABLED** (commented out) — Amazon blocks subtitles in headless mode
4. **Create context chain**: `ExecAllocator` → `chromedp.NewContext` → `context.WithTimeout(6 minutes)`
5. **Cleanup function**: cancels all contexts in reverse order, then `os.RemoveAll(tmpDir)`

### 4.5 EPISODE NAVIGATION

#### `navigateToEpisode(ctx)`
Orchestrates the full navigation sequence:
1. `chromedp.Navigate(showURL)` → `WaitReady("body")` → sleep 3s
2. `selectSeason(ctx)` → sleep 4s
3. `clickEpisode(ctx)` → sleep 3s
4. `clickPlay(ctx)` → sleep 3s

#### `selectSeason(ctx)`
Injects a JavaScript function that tries three strategies:
1. **`<select>` dropdown**: Find all `<select>` elements, iterate options, match text against regex `/(?:Season|Series)\s*N(?:\s|$|\b)/i`, set `selectedIndex` and dispatch `change` event
2. **Data-attribute triggers**: Click elements with `data-automation-id*="season"`, `data-testid*="season"`, `class*="eason"` button/role attributes
3. **Text matching**: Find `a`, `button`, `li`, `role="option"`, `role="tab"`, `role="menuitem"` elements whose text matches the Season regex and is < 40 chars

Returns `"select"`, `"click"`, or `"not_found"`. Logs the result.

#### `clickEpisode(ctx)`
**Retry loop**: Up to 6 attempts with 2.5s sleep between attempts. Injects JS with three strategies:

1. **Position-based (primary)**: Query all `a[href*="/detail/"]` links. Deduplicate by href. Filter to those inside episode card containers (`data-testid*="episode"`, `class*="pisode"`, `section`, `article`, `li`) OR containing an `<img>`. If not enough, try links with `ref=` in href, excluding trailers/bonus. Click the Nth link (N = targetEpisode).
2. **Data-testid cards**: Try selectors `[data-testid^="title-card-"]`, `[data-testid^="card-title-"]`, `[data-testid^="episode-card-"]`. Click the Nth card's link.
3. **Text-pattern matching**: Match against `/\bEN\b/`, `/\bEpisode\s+N\b/i`, `/S\d+\s*EN\b/i` in `a[href*="detail"], a[href*="video"]` elements.
4. **Debug output**: If all fail, returns diagnostic info (count of unique links, first 12 pathnames, count of episode data attributes).

#### `clickPlay(ctx)`
Injects JS with three priorities:
1. `a[href*="/watch/"]` links (direct player links)
2. Buttons/links with play-related data-testid or automation-id attributes, or `a[href*="autoplay=1"]`
3. Text match: find `a` or `button` with exact text "play", "watch now", "watch", "resume", "play episode", "watch episode" — explicitly excluding "watchlist"

#### `enableSubtitles(ctx)`
Waits 3s for player to load, then:
1. **Find CC button**: by `aria-label*="ubtitle"`, `aria-label*="losed caption"`, `aria-label*="CC"`, `data-testid*="subtitle"`, `class*="subtitle"`, `class*="caption"`
2. **Player controls fallback**: Search `.atvwebplayersdk-controls button` and similar for subtitle/caption/cc labels
3. **Keyboard fallback**: Dispatch `keydown` event for key `'c'` (CC toggle shortcut)
4. **Language selection** (if CC button was found): Wait 1.5s, then look for `role="menuitemradio"`, `role="option"`, subtitle/caption `li` items. Click the one containing "english", "en_gb", or "[cc]". If none found, click the first available option.
5. Final 5s sleep for subtitles to start loading.

### 4.6 NETWORK MONITOR

#### `startNetworkListener(ctx) *monitorState`
Creates a `monitorState` with empty maps and a `done` channel. Registers a `chromedp.ListenTarget` callback that handles two CDP event types:

**`network.EventResponseReceived`**:
- If URL contains `"GetPlaybackResources"` → store request ID in `mon.apiReqIDs`
- If URL contains `"ttml"`, `"subtitle"`, `"timedtext"`, `".mpd"`, or `"manifest"` → log it for debugging
- If URL contains `subtitleExt` (`.ttml2`) → store request ID → URL mapping in `urlIndex`

**`network.EventLoadingFinished`**:
- If the request ID was tracked in `urlIndex` → create a `capturedSub` entry in `mon.responses` with the URL and `EncodedDataLength`

#### `startSettleTimer(ctx, mon)`
Runs a goroutine that implements a **settle-based wait**:
- **Smart shortcut**: If API responses were already captured but no direct `.ttml2` files, reduce timeout to 10s (the subtitles will be extracted from the API response instead)
- **Ticker loop** (500ms): Checks if `len(mon.responses) > 0` AND `time.Since(lastSeen) >= settleTime` (8s of no new responses) → close `done` channel
- **Hard timeout**: `maxWaitTime` (90s)
- Uses `safeClose()` helper to avoid double-close panics on the channel

### 4.7 RESPONSE SELECTION & DOWNLOAD

#### `selectLargestOrNil(responses) *capturedSub`
Iterates all captured responses and returns the one with the largest `size`. Returns `nil` if map is empty.

#### `extractSubURLFromAPI(ctx, mon) []string`
Reads the body of each captured `GetPlaybackResources` response and extracts English `.ttml2` URLs:

1. **JSON structured parse** (`findEnglishSubInJSON`): Parse the response as JSON and look for the `subtitleUrls` array. For each entry, check `languageCode` starts with "en", check URL contains ".ttml2", check `type` field for "sdh" vs "subtitle". Prefer SDH track when `preferSDH` is true (SDH tracks include speaker labels like `[Homelander]`). Falls back to recursive JSON search (`findSubURLsRecursive`) that walks arbitrary nested JSON looking for objects with `languageCode`/`language`/`locale` fields starting with "en" and a `url` field containing ".ttml2".
2. **Content validation**: Checks `catalogMetadata.catalog.runtimeSeconds` — if < `minRuntimeSecs`, logs a warning that this might be a trailer.
3. **Regex fallback**: If JSON parsing finds nothing, regex-extract all `https?://...\.ttml2...` URLs from the raw body. For each match, examine 200 chars of surrounding context for English language markers (`"en_gb"`, `"en_us"`, `"en"`, `"english"`). If no English context found, take the last URL as a fallback.
4. Saves the raw API response to `api_response.json` for debugging.

#### `downloadSubtitleHTTP(url string) []byte`
Standard `http.Client.Get` with 30s timeout. Fatal on non-200 status.

#### `downloadBody(ctx, sub) []byte`
Uses `network.GetResponseBody(sub.requestID).Do(ctx)` to retrieve the response body from Chrome's network cache.

### 4.8 TTML2 PARSER

#### `parseTTML2(data []byte) string`
A streaming XML parser using `encoding/xml.NewDecoder`. Processes tokens:
- **`<p>` start element**: Increment depth counter. Extract `begin` attribute → format timestamp (strip milliseconds: `"00:01:23.456"` → `"00:01:23"`). Prepend `[00:01:23] ` to the output line.
- **`<br>` inside `<p>`**: Emit a space (join multi-line cues on one line)
- **`CharData` inside `<p>`**: Trim whitespace, append text + space
- **`</p>`**: Decrement depth, reset timestamp, emit newline

Output format per line: `[HH:MM:SS] dialogue text here`

### 4.9 GEMINI API

#### Data types
```go
geminiRequest       { Contents []geminiContent, GenerationConfig *geminiGenerationConfig }
geminiContent       { Parts []geminiPart }
geminiPart          { Text string }
geminiGenerationConfig { ThinkingConfig *thinkingConfig, MaxOutputTokens int }
thinkingConfig      { ThinkingBudget int }
geminiResponse      { Candidates []{ Content { Parts []{ Text string, Thought bool } } }, Error *{ Message string, Code int } }
```

#### `callGemini(apiKey, content string) string`
1. Build request body with `ThinkingBudget: 10000` and `MaxOutputTokens: 65536`
2. Construct URL: `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent?key={key}`
3. Use a shared `http.Transport` with `MaxIdleConns: 2`, `ForceAttemptHTTP2: true`, `IdleConnTimeout: 90s`
4. **Retry loop** (max 4 attempts):
   - On HTTP 429: exponential backoff (`30 * attempt` seconds), retry
   - On non-200: fatal with response body
   - On success: decode JSON, check for API error, filter out `Thought: true` parts (internal reasoning), concatenate remaining text parts

### 4.10 UTILITIES

```go
func resolveAPIKey() string        // hardcoded → env GEMINI_API_KEY → fatal
func mustRun(ctx, ...Action)       // chromedp.Run wrapper, fatal on error
func writeFile(path, content)      // os.WriteFile wrapper, fatal on error
func truncate(s string, max int)   // truncate with "..." suffix
func copyFile(src, dst string)     // silent copy, ignores errors (used for profile cloning)
```

---

## 5. IMPORTS (main.go)

```go
import (
    "bytes"
    "context"
    "encoding/json"
    "encoding/xml"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "sync"
    "time"

    "github.com/chromedp/cdproto/network"
    "github.com/chromedp/chromedp"
)
```

---

## 6. DATA FLOW DIAGRAM

```
┌──────────────┐     ┌──────────────────┐     ┌───────────────────┐
│  User's Real │────►│  Temp Chrome     │────►│  Amazon Prime     │
│  Chrome      │copy │  Profile Dir     │load │  Video Page       │
│  Profile     │     │  (session files) │     │  (show detail)    │
└──────────────┘     └──────────────────┘     └────────┬──────────┘
                                                       │
                                              JS injection:
                                              selectSeason()
                                              clickEpisode()
                                              clickPlay()
                                              enableSubtitles()
                                                       │
                                                       ▼
                                              ┌────────────────────┐
                                              │  CDP Network       │
                                              │  Event Listener    │
                                              │                    │
                                              │  Path A: .ttml2    │──► GetResponseBody
                                              │  responses         │    → raw TTML2 XML
                                              │                    │
                                              │  Path B: API resp  │──► Extract subtitle
                                              │  GetPlayback       │    URLs from JSON
                                              │  Resources         │    → HTTP GET each
                                              └────────┬───────────┘
                                                       │
                                                       ▼
                                              ┌────────────────────┐
                                              │  parseTTML2()      │
                                              │  XML → timestamped │
                                              │  plain text        │
                                              └────────┬───────────┘
                                                       │
                                    ┌──────────────────┼──────────────────┐
                                    ▼                                     ▼
                           subtitle_output.txt                   Gemini API POST
                                                                  (forensic prompt
                                                                   + subtitle text)
                                                                         │
                                                                         ▼
                                                                analysis_output.txt
```

---

## 7. AUXILIARY FILE: dump.go

A **separate** `main` package file (only one can be built at a time — use build tags or rename). Purpose: debug tool to dump all `<a>` and `<button>` elements from the Amazon page.

- Launches Chrome in **headless** mode with a debug temp profile dir
- Navigates to the show URL, sleeps 5s
- Evaluates JS: `Array.from(document.querySelectorAll('a, button')).map(el => tagName + " | " + href + " | " + innerText).join('\n')`
- Writes result to `dom_dump.txt`

> **Note**: This file has its own `func main()` and conflicts with `main.go`. To build the main app, either rename `dump.go` or use a build tag. They cannot coexist in the same build.

---

## 8. AUXILIARY FILE: prime_subtitle_grabber.js

A standalone **Node.js + Puppeteer** implementation that does the same subtitle capture but from JavaScript. Key differences from the Go version:

- Uses Puppeteer instead of chromedp
- Uses a persistent `userDataDir` for login cookies (no profile cloning needed)
- Blocks ad/tracking domains via `Network.setBlockedURLs` for speed
- Uses `domcontentloaded` instead of `networkidle2` for faster navigation
- Handles manual login: if redirected to `/ap/(signin|mfa)`, waits for user to sign in
- Season selection: tries `<select>` dropdown then tab/button click
- Episode selection: index-based card clicking, then text-match fallback
- Player detection: races `waitForSelector` on original page vs new tabs
- Caption enabling: clicks CC button selectors then picks first menu item
- Subtitle capture: listens for responses from `cf-timedtext.aux.pv-cdn.net` containing `ttml`
- **Resolves immediately** on first subtitle capture (no settle timer needed)
- Saves files with timestamp prefix: `{Date.now()}_{filename}.ttml2`

---

## 9. CRITICAL EDGE CASES & DESIGN DECISIONS

1. **Profile Cloning**: Chrome locks its profile database. You cannot use the real profile directly while Chrome is open. Solution: copy only the essential session files (Cookies, Login Data, etc.) to a temp dir.

2. **Headless vs Headed**: Amazon blocks subtitle delivery in headless Chrome. The app MUST run headed.

3. **`disable-blink-features=AutomationControlled`**: Without this, Amazon detects automation and may block content.

4. **Two-path subtitle capture**: Direct `.ttml2` interception is fastest, but Amazon sometimes embeds subtitle URLs inside the `GetPlaybackResources` JSON response instead of making separate network requests. The fallback extracts URLs from that JSON.

5. **SDH preference**: SDH (Subtitles for the Deaf and Hard-of-hearing) tracks include speaker labels like `[Homelander]` and sound descriptions like `[explosion]`, which are critical for the forensic analysis prompt. Plain subtitle tracks lack these.

6. **Trailer detection**: The `GetPlaybackResources` response includes `catalogMetadata.catalog.runtimeSeconds`. If this is < 300s (5 min), warn that it's likely a trailer, not an episode.

7. **Settle timer**: Subtitles may arrive as multiple fragments. The settle timer waits for 8s of network quiescence after the last response before declaring capture complete.

8. **Rate limit handling**: Gemini free tier has aggressive rate limits. The app retries up to 4 times with linear backoff (30s, 60s, 90s, 120s).

9. **Thinking budget**: Gemini 2.5 Flash supports a `thinkingBudget` parameter. Set to 10000 tokens for deep analysis. The response may include `Thought: true` parts (internal reasoning) which are filtered out of the final output.

10. **Channel safety**: The `safeClose()` helper prevents panics from double-closing the `done` channel (which can happen if both the timeout and the settle condition fire simultaneously).

---

## 10. BUILD & RUN INSTRUCTIONS

```bash
# Initialize module
go mod init polymovie
go mod tidy

# Build (rename dump.go first to avoid conflict)
# Either: rename dump.go to dump.go.bak
# Or: use build tag
go build -o polymovie.exe .

# Run
./polymovie.exe

# Prerequisites:
# - Chrome installed at default location
# - User logged into Amazon Prime Video in Chrome's Default profile
# - Chrome must NOT be running (or profile lock will block copy)
# - Gemini API key valid and not rate-limited
```

---

## 11. OUTPUT FORMAT

**subtitle_output.txt** — One line per cue:
```
[00:00:04] Go the fuck away!
[00:00:11] Oh, no, please!
[00:00:16] Homelander, what made you decide to join up with Vought?
[00:00:20] A chance to use my gifts to help make the world a better place.
```

**analysis_output.txt** — Gemini's forensic report with sections:
1. Executive Summary
2. Character-by-Character Death Ledger (11 characters)
3. Chronological Death Tracker
4. Ambiguities and Contradictions
5. Final Verdict

---

*End of reconstruction prompt. An LLM given this document should be able to reproduce the entire Polymovie codebase with full functional fidelity.*
