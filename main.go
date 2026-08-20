package main

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

// ==========================================================================
//  CONFIGURATION
// ==========================================================================
const (
	// -- Amazon Prime Video --
	showURL       = "https://www.amazon.co.uk/gp/video/detail/B0DWSGK5GS/"
	targetSeason  = 1
	targetEpisode = 1

	// -- Chrome Profile --
	chromeUserDataDir = `C:\Users\isaam\AppData\Local\Google\Chrome\User Data`
	chromeProfile     = "Default"

	// -- Subtitle Capture --
	subtitleExt = ".ttml2"
	maxWaitTime = 90 * time.Second
	settleTime  = 8 * time.Second

	// -- Gemini API (free tier) --
	// Leave empty; the key is read from the GEMINI_API_KEY env var.
	hardcodedAPIKey = ""
	geminiModel     = "gemini-2.5-flash"
	apiTimeout      = 5 * time.Minute

	// -- Prompt --
	gptPrompt = `You are a forensic script analyst for The Boys. Your job is to read the provided episode script(s) in depth and track the death status of these characters across the story: Homelander, Billy Butcher, Starlight, Hughie, Soldier Boy, Ashley, Mother's Milk (MM), The Deep, Sister Sage, Ryan Butcher, and Kimiko Miyashiro.

Your output must be grounded only in the script text I provide. Do not guess. Do not rely on outside memory unless the script itself explicitly supports it. Read carefully for dialogue, stage directions, sound cues, scene transitions, repeated references, and emotional framing.

Your task is not just to list who dies. You must distinguish among:
1. Confirmed physical death
2. Implied death
3. Fake-out death
4. Fatal or near-fatal injury
5. Sacrifice scene
6. Survival with major risk
7. Missing/unknown status
8. Hallucination, impersonation, clone, alternate version, or psychic construct
9. Thematic or symbolic death that is not literal

For each character, determine their status at every episode or script segment available. Track the status chronologically. If a character is alive at one point, later dies, and later appears again, explain whether the later appearance is a flashback, hallucination, resurrection, clone, memory, vision, or a continuity contradiction.

You must read the script carefully and cite exact evidence from the text. For every major claim, quote the relevant lines exactly and explain why they support your conclusion. If a death is only implied, explain why it is not fully confirmed. If the script uses off-screen violence, cutaways, reaction shots, silence, music cues, or body language to suggest death, explain that too.

Pay special attention to:
- Explicit words like "dead," "die," "killed," "gone," "lost," "no pulse," "won't make it," "never coming back," or similar wording
- Stage directions such as screams, choking, silence, collapse, blood, gunshots, neck cracks, explosions, or sudden music shifts
- Emotional farewell scenes that may function as death scenes
- Characters being removed from the story, abandoned, or left behind
- Superpowered healing, regeneration, resurrection, or body possession
- Homelander-style execution scenes, intimidation scenes, and cult-like loyalty tests
- Any scene where the script implies a death but later dialogue contradicts it

When analyzing each character, use this checklist:
- First appearance in the script
- Last confirmed alive moment
- Any explicit death confirmation
- Any implied death or fake-out
- Cause of death or injury, if any
- Who is present during the scene
- Whether the death is on-screen, off-screen, or only reported
- Confidence level: high, medium, or low
- Any contradictions or unresolved ambiguities

Your final answer must be structured like a forensic report with these sections:

1. Executive summary
   - One short paragraph on the overall death landscape in the script
   - State which deaths are confirmed, implied, or unresolved

2. Character-by-character death ledger
   For each of the 11 characters, include:
   - Status: alive / dead / implied dead / unknown / fake-out / ambiguous
   - Evidence from the script
   - Explanation of why that status is warranted
   - Confidence level
   - Timeline notes if the status changes

3. Chronological death tracker
   - A timeline of every death-related event in order
   - Include episode or timestamp if available
   - Note the exact scene and what changed

4. Ambiguities and contradictions
   - Any scene that looks like death but might not be
   - Any later scene that complicates earlier conclusions
   - Any character whose "death" is symbolic rather than literal

5. Final verdict
   - A concise list of who is definitely dead, who is probably dead, who is alive, and who remains uncertain

Rules:
- Never overstate certainty.
- Separate literal death from symbolic death.
- Separate on-screen confirmation from off-screen implication.
- If the script does not prove death, say so clearly.
- If a character is only mentioned as dead in dialogue, distinguish that from seeing the death happen.
- If a character appears after an apparent death, explain the resolution.
- Quote the script precisely and sparingly, but enough to support the conclusion.
- Do not summarize the plot except where necessary to explain a death.
- Do not ignore a scene just because it is emotionally heavy; analyze whether it is actually a death scene.
- Be especially careful with Frenchie-like farewell scenes, fake-outs, and ambiguous survivals.

Before answering, read the full script in depth and re-check any death-related scene at least twice for context. If the script is truncated or incomplete, say exactly what is missing and how that limits confidence.

Now analyze the provided script(s) and produce the report.

--- SUBTITLE TEXT BEGINS ---
`

	// -- Output --
	subtitleFile = "subtitle_output.txt"
	analysisFile = "analysis_output.txt"
)

// ==========================================================================

type capturedSub struct {
	requestID network.RequestID
	url       string
	size      float64
}

func main() {
	start := time.Now()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Println("=== Polymovie Subtitle Extractor ===")

	apiKey := resolveAPIKey()
	if showURL == "" {
		log.Fatal("[X] Set the showURL constant in main.go before running")
	}

	ctx, cleanup := launchChrome()
	defer cleanup()

	log.Println("[>] Enabling network monitoring...")
	mustRun(ctx, network.Enable())

	// Start listening for subtitle network events immediately
	mon := startNetworkListener(ctx)

	navigateToEpisode(ctx)

	// Try enabling CC/subtitles in the player
	enableSubtitles(ctx)

	// NOW start the settle timer (after navigation is done)
	log.Println("[>] Waiting for subtitle network response...")
	startSettleTimer(ctx, mon)
	<-mon.done

	var body []byte

	// Path 1: subtitle was captured directly from network
	if largest := selectLargestOrNil(mon.responses); largest != nil {
		log.Printf("[>] Downloading captured subtitle response (%.1f KB)...", largest.size/1024)
		body = downloadBody(ctx, largest)
	} else {
		// Path 2: extract subtitle URL from GetPlaybackResources API response
		log.Println("[>] No .ttml2 captured directly, extracting URLs from API response...")
		subURLs := extractSubURLFromAPI(ctx, mon)
		if len(subURLs) == 0 {
			log.Fatal("[X] Could not find subtitle URL in API responses.\n" +
				"  Possible causes:\n" +
				"  - Chrome login session expired (re-login in Chrome first)\n" +
				"  - Subtitles not available for this episode\n" +
				"  - Episode URL is incorrect")
		}
		var combinedBody []byte
		for i, u := range subURLs {
			log.Printf("[>] Downloading subtitle URL %d/%d via HTTP...", i+1, len(subURLs))
			dl := downloadSubtitleHTTP(u)
			if len(dl) > 0 {
				log.Printf("  [OK] Downloaded %d bytes via HTTP", len(dl))
				combinedBody = append(combinedBody, dl...)
				combinedBody = append(combinedBody, []byte("\n")...)
			}
		}
		if len(combinedBody) > 0 {
			body = combinedBody
		}
	}

	log.Printf("[>] Got a total of %d bytes of TTML2 data", len(body))

	subtitleText := parseTTML2(body)
	if subtitleText == "" {
		log.Fatal("[X] Parsed TTML2 but got empty text")
	}
	log.Printf("[>] Extracted %d characters of subtitle text", len(subtitleText))

	writeFile(subtitleFile, subtitleText)
	log.Printf("[>] Saved subtitles -> %s", subtitleFile)

	log.Printf("[>] Sending %d chars to Gemini %s ...", len(subtitleText), geminiModel)
	analysis := callGemini(apiKey, gptPrompt+subtitleText)

	writeFile(analysisFile, analysis)
	log.Printf("[>] Saved analysis -> %s", analysisFile)

	elapsed := time.Since(start)
	log.Printf("[OK] Completed in %s", elapsed.Round(time.Millisecond))
	log.Printf("  Subtitles : %s (%d chars)", subtitleFile, len(subtitleText))
	log.Printf("  Analysis  : %s (%d chars)", analysisFile, len(analysis))
}

// ==========================================================================
//  CHROME LAUNCHER - clones profile to temp dir so Chrome can stay open
// ==========================================================================

func launchChrome() (context.Context, func()) {
	tmpDir, err := os.MkdirTemp("", "polymovie-profile-*")
	if err != nil {
		log.Fatalf("[X] Failed to create temp profile dir: %v", err)
	}
	log.Printf("[>] Cloning Chrome session -> %s", tmpDir)

	srcProfile := filepath.Join(chromeUserDataDir, chromeProfile)
	dstProfile := filepath.Join(tmpDir, chromeProfile)
	os.MkdirAll(dstProfile, 0755)

	// Copy all critical session files including cookie journals and network state
	for _, name := range []string{
		"Cookies", "Cookies-journal",
		"Login Data", "Login Data-journal",
		"Web Data", "Web Data-journal",
		"Preferences", "Secure Preferences",
		"Network Action Predictor",
		"TransportSecurity",
	} {
		copyFile(filepath.Join(srcProfile, name), filepath.Join(dstProfile, name))
	}
	// Also copy Network subdir if it exists (contains cookies in newer Chrome)
	netSrc := filepath.Join(srcProfile, "Network")
	netDst := filepath.Join(dstProfile, "Network")
	if info, err := os.Stat(netSrc); err == nil && info.IsDir() {
		os.MkdirAll(netDst, 0755)
		for _, name := range []string{"Cookies", "Cookies-journal", "TransportSecurity"} {
			copyFile(filepath.Join(netSrc, name), filepath.Join(netDst, name))
		}
	}
	copyFile(filepath.Join(chromeUserDataDir, "Local State"), filepath.Join(tmpDir, "Local State"))

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(tmpDir),
		chromedp.Flag("profile-directory", chromeProfile),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-translate", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("no-first-run", true),
		// chromedp.Flag("headless", "new"), // DISABLED: Amazon may block subtitles in headless
		chromedp.WindowSize(1920, 1080),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	deadlineCtx, deadlineCancel := context.WithTimeout(taskCtx, 6*time.Minute)

	cleanup := func() {
		deadlineCancel()
		taskCancel()
		allocCancel()
		os.RemoveAll(tmpDir)
	}
	return deadlineCtx, cleanup
}

// ==========================================================================
//  EPISODE NAVIGATION
// ==========================================================================

func navigateToEpisode(ctx context.Context) {
	log.Printf("[>] Navigating to show: %s", truncate(showURL, 100))
	mustRun(ctx, chromedp.Navigate(showURL))
	mustRun(ctx, chromedp.WaitReady("body"))
	mustRun(ctx, chromedp.Sleep(3*time.Second))

	selectSeason(ctx)
	mustRun(ctx, chromedp.Sleep(4*time.Second))

	clickEpisode(ctx)
	mustRun(ctx, chromedp.Sleep(3*time.Second))

	clickPlay(ctx)
	mustRun(ctx, chromedp.Sleep(3*time.Second))
}

func selectSeason(ctx context.Context) {
	log.Printf("[>] Selecting Season %d...", targetSeason)

	js := fmt.Sprintf(`(function() {
		var s = %d;
		var re = new RegExp('(?:Season|Series)\\s*' + s + '(?:\\s|$|\\b)', 'i');
		var sels = document.querySelectorAll('select');
		for (var i = 0; i < sels.length; i++) {
			for (var j = 0; j < sels[i].options.length; j++) {
				if (re.test(sels[i].options[j].textContent)) {
					sels[i].selectedIndex = j;
					sels[i].dispatchEvent(new Event('change', {bubbles:true}));
					return 'select';
				}
			}
		}
		var triggers = document.querySelectorAll(
			'[data-automation-id*="season"], [data-testid*="season"], ' +
			'[class*="eason"] button, [class*="eason"] [role="button"]'
		);
		if (triggers.length) triggers[0].click();
		var els = document.querySelectorAll(
			'a, button, li, [role="option"], [role="tab"], [role="menuitem"]'
		);
		for (var i = 0; i < els.length; i++) {
			var t = els[i].textContent.trim();
			if (re.test(t) && t.length < 40) { els[i].click(); return 'click'; }
		}
		return 'not_found';
	})()`, targetSeason)

	var result string
	mustRun(ctx, chromedp.Evaluate(js, &result))

	switch result {
	case "select":
		log.Printf("  [OK] Season %d selected (dropdown)", targetSeason)
	case "click":
		log.Printf("  [OK] Season %d selected (click)", targetSeason)
	default:
		log.Printf("  [!] Season selector not found - assuming Season %d is already active", targetSeason)
	}
}

func clickEpisode(ctx context.Context) {
	log.Printf("[>] Looking for Episode %d (will retry for up to 15s)...", targetEpisode)

	js := fmt.Sprintf(`(function() {
		var ep = %d;

		// STRATEGY 1: Position-based via unique detail links
		var detailLinks = document.querySelectorAll('a[href*="/detail/"]');
		var seen = {};
		var unique = [];
		for (var i = 0; i < detailLinks.length; i++) {
			var h = detailLinks[i].href;
			if (!h || seen[h]) continue;
			var parent = detailLinks[i].closest('[data-testid*="episode"], [data-testid*="card"], [class*="pisode"], [class*="Card"], section, article, li');
			if (parent || detailLinks[i].querySelector('img')) {
				seen[h] = true;
				unique.push(detailLinks[i]);
			}
		}
		if (unique.length === 0) {
			seen = {};
			for (var i = 0; i < detailLinks.length; i++) {
				var h = detailLinks[i].href;
				if (!h || seen[h] || !h.includes('ref=')) continue;
				var text = (detailLinks[i].textContent || "").toLowerCase();
				if (text.includes('trailer') || text.includes('bonus')) continue;
				seen[h] = true;
				unique.push(detailLinks[i]);
			}
		}
		// We don't filter by pathname because episode cards might share the same path as the season.
		if (unique.length >= ep) {
			var link = unique[ep - 1];
			link.click();
			return 'clicked:' + link.href + ' [position ' + ep + '/' + unique.length + ']';
		}

		// STRATEGY 2: data-testid card containers
		var cardSels = ['[data-testid^="title-card-"]', '[data-testid^="card-title-"]', '[data-testid^="episode-card-"]'];
		for (var cs = 0; cs < cardSels.length; cs++) {
			var cards = document.querySelectorAll(cardSels[cs]);
			if (cards.length >= ep) {
				var card = cards[ep - 1];
				var a = card.querySelector('a') || card.closest('a');
				if (a) { a.click(); return 'clicked:' + (a.href||'') + ' [card-sel]'; }
				card.click(); return 'clicked:card-position';
			}
		}

		// STRATEGY 3: Text-pattern matching
		var pats = [
			new RegExp('\\bE' + ep + '\\b'),
			new RegExp('\\bEpisode\\s+' + ep + '\\b', 'i'),
			new RegExp('S\\d+\\s*E' + ep + '\\b', 'i'),
		];
		var textLinks = document.querySelectorAll('a[href*="detail"], a[href*="video"]');
		for (var i = 0; i < textLinks.length; i++) {
			var txt = textLinks[i].textContent || '';
			for (var p = 0; p < pats.length; p++) {
				if (pats[p].test(txt)) {
					textLinks[i].click();
					return 'clicked:' + (textLinks[i].href||'') + ' [text-match]';
				}
			}
		}

		// DEBUG
		var d = [];
		d.push('unique_detail_links=' + unique.length);
		var samples = [];
		for (var i = 0; i < unique.length && i < 12; i++) {
			samples.push('[' + i + '] ' + unique[i].pathname.substring(0,50));
		}
		d.push('links: ' + samples.join(' | '));
		d.push('ep_data_attrs=' + document.querySelectorAll('[data-testid*="episode"]').length);
		return 'debug:' + d.join('; ');
	})()`, targetEpisode)

	const maxAttempts = 6
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var result string
		mustRun(ctx, chromedp.Evaluate(js, &result))

		if strings.HasPrefix(result, "clicked:") {
			log.Printf("  [OK] Episode %d -> %s", targetEpisode, truncate(result, 150))
			return
		}
		if attempt < maxAttempts {
			log.Printf("  [~] Attempt %d/%d - %s", attempt, maxAttempts, truncate(result, 120))
			mustRun(ctx, chromedp.Sleep(2500*time.Millisecond))
		} else {
			log.Printf("  [X] Final debug: %s", result)
			log.Fatalf("[X] Episode %d not found after %d attempts.", targetEpisode, maxAttempts)
		}
	}
}

func clickPlay(ctx context.Context) {
	log.Println("[>] Clicking Play to trigger subtitle loading...")

	js := `(function() {
		// Priority 1: Direct /watch/ links (these go to the player)
		var watchLinks = document.querySelectorAll('a[href*="/watch/"]');
		if (watchLinks.length > 0) {
			watchLinks[0].click();
			return 'clicked:' + watchLinks[0].href;
		}

		// Priority 2: Buttons/links with play-related data attributes
		var playBtns = document.querySelectorAll(
			'[data-testid*="play-button"], [data-testid*="playback"], ' +
			'[data-automation-id*="play"], ' +
			'a[href*="autoplay=1"]'
		);
		if (playBtns.length > 0) {
			playBtns[0].click();
			return 'clicked:' + (playBtns[0].href || playBtns[0].getAttribute('data-testid'));
		}

		// Priority 3: Exact text match (avoid "watchlist", "add to watchlist")
		var all = document.querySelectorAll('a, button');
		for (var i = 0; i < all.length; i++) {
			var t = all[i].textContent.trim().toLowerCase();
			if (t.indexOf('watchlist') !== -1) continue;
			if (t === 'play' || t === 'watch now' || t === 'watch' ||
				t === 'resume' || t === 'play episode' ||
				t === 'watch episode') {
				all[i].click();
				return 'clicked:' + t;
			}
		}
		return 'not_found';
	})()`

	var result string
	mustRun(ctx, chromedp.Evaluate(js, &result))

	if strings.HasPrefix(result, "clicked:") {
		log.Printf("  [OK] Play -> %s", truncate(result, 100))
	} else {
		log.Println("  [!] Play button not found - subtitles may still preload on some pages")
	}
}

func enableSubtitles(ctx context.Context) {
	log.Println("[>] Enabling subtitles/CC in player...")
	mustRun(ctx, chromedp.Sleep(3*time.Second))

	js := `(function() {
		// Strategy 1: Click CC/subtitle button by aria-label or data attributes
		var ccBtns = document.querySelectorAll(
			'[aria-label*="ubtitle" i], [aria-label*="losed caption" i], ' +
			'[aria-label*="CC" i], [data-testid*="subtitle" i], ' +
			'[class*="subtitle" i], [class*="caption" i]'
		);
		for (var i = 0; i < ccBtns.length; i++) {
			ccBtns[i].click();
			return 'clicked_cc:' + (ccBtns[i].getAttribute('aria-label') || ccBtns[i].className);
		}

		// Strategy 2: Look in the player controls area for subtitle icon (often an SVG)
		var playerBtns = document.querySelectorAll('.atvwebplayersdk-controls button, .fkpovp9 button, [class*="player"] button');
		for (var i = 0; i < playerBtns.length; i++) {
			var lbl = (playerBtns[i].getAttribute('aria-label') || playerBtns[i].title || '').toLowerCase();
			if (lbl.indexOf('subtitle') !== -1 || lbl.indexOf('caption') !== -1 || lbl.indexOf('cc') !== -1) {
				playerBtns[i].click();
				return 'clicked_player_btn:' + lbl;
			}
		}

		// Strategy 3: Try keyboard shortcut 'c' for CC toggle
		document.dispatchEvent(new KeyboardEvent('keydown', {key: 'c', code: 'KeyC', bubbles: true}));
		return 'sent_keyboard_c';
	})()`

	var result string
	mustRun(ctx, chromedp.Evaluate(js, &result))
	log.Printf("  [>] Subtitle enable result: %s", result)

	// If we got a CC button, look for a language option to click
	if strings.HasPrefix(result, "clicked_cc") || strings.HasPrefix(result, "clicked_player") {
		mustRun(ctx, chromedp.Sleep(1500*time.Millisecond))
		langJS := `(function() {
			var opts = document.querySelectorAll('[role="menuitemradio"], [role="option"], [class*="subtitle"] li, [class*="caption"] li');
			for (var i = 0; i < opts.length; i++) {
				var t = opts[i].textContent.toLowerCase();
				if (t.indexOf('english') !== -1 || t.indexOf('en_gb') !== -1 || t.indexOf('[cc]') !== -1) {
					opts[i].click();
					return 'selected:' + opts[i].textContent.trim();
				}
			}
			// Click first available option if no English found
			if (opts.length > 0) {
				opts[0].click();
				return 'selected_first:' + opts[0].textContent.trim();
			}
			return 'no_options';
		})()`
		var langResult string
		mustRun(ctx, chromedp.Evaluate(langJS, &langResult))
		log.Printf("  [>] Language selection: %s", langResult)
	}

	// Give time for subtitles to start loading
	mustRun(ctx, chromedp.Sleep(5*time.Second))
}

// ==========================================================================
//  NETWORK MONITOR
// ==========================================================================

type monitorState struct {
	mu         sync.Mutex
	responses  map[network.RequestID]*capturedSub
	apiReqIDs  []network.RequestID // GetPlaybackResources request IDs
	done       chan struct{}
}

func startNetworkListener(ctx context.Context) *monitorState {
	mon := &monitorState{
		responses: make(map[network.RequestID]*capturedSub, 8),
		done:      make(chan struct{}),
	}
	urlIndex := make(map[network.RequestID]string, 16)

	isSubtitleURL := func(url string) bool {
		return strings.Contains(url, subtitleExt)
	}

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			url := e.Response.URL
			if strings.Contains(url, "GetPlaybackResources") {
				mon.mu.Lock()
				mon.apiReqIDs = append(mon.apiReqIDs, e.RequestID)
				mon.mu.Unlock()
				log.Printf("  [API] GetPlaybackResources captured (reqID: %s)", e.RequestID)
			}
			if strings.Contains(url, "ttml") ||
				strings.Contains(url, "subtitle") ||
				strings.Contains(url, "timedtext") ||
				strings.Contains(url, ".mpd") ||
				strings.Contains(url, "manifest") {
				log.Printf("  [NET] %d %s %s", e.Response.Status, e.Response.MimeType, truncate(url, 120))
			}
			if !isSubtitleURL(url) {
				return
			}
			mon.mu.Lock()
			urlIndex[e.RequestID] = url
			mon.mu.Unlock()
			log.Printf("  [!] Subtitle detected: %s", truncate(url, 100))

		case *network.EventLoadingFinished:
			mon.mu.Lock()
			url, tracked := urlIndex[e.RequestID]
			if tracked {
				mon.responses[e.RequestID] = &capturedSub{
					requestID: e.RequestID,
					url:       url,
					size:      e.EncodedDataLength,
				}
				delete(urlIndex, e.RequestID)
				log.Printf("  [OK] Loaded: %.1f KB", e.EncodedDataLength/1024)
			}
			mon.mu.Unlock()
		}
	})

	return mon
}

// startSettleTimer starts the countdown. Call this AFTER navigation is done.
func startSettleTimer(ctx context.Context, mon *monitorState) {
	go func() {
		lastSeen := time.Now()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.After(maxWaitTime)

		// Check if API responses already captured (they arrive during navigation)
		mon.mu.Lock()
		hasAPI := len(mon.apiReqIDs) > 0
		hasSubs := len(mon.responses) > 0
		mon.mu.Unlock()

		if hasAPI && !hasSubs {
			// API captured but no direct .ttml2 — don't wait the full 90s
			log.Println("  [>] API response already captured, using short wait...")
			timeout = time.After(10 * time.Second)
		}

		for {
			select {
			case <-ctx.Done():
				safeClose(mon.done)
				return
			case <-timeout:
				log.Println("  [!] Max wait reached")
				safeClose(mon.done)
				return
			case <-ticker.C:
				mon.mu.Lock()
				count := len(mon.responses)
				mon.mu.Unlock()
				if count > 0 && time.Since(lastSeen) >= settleTime {
					log.Printf("  [OK] Network settled (%d responses)", count)
					safeClose(mon.done)
					return
				}
			}
		}
	}()
}

func safeClose(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// ==========================================================================
//  RESPONSE SELECTION & DOWNLOAD
// ==========================================================================

func selectLargestOrNil(responses map[network.RequestID]*capturedSub) *capturedSub {
	var largest *capturedSub
	for _, r := range responses {
		if largest == nil || r.size > largest.size {
			largest = r
		}
	}
	return largest
}

// extractSubURLFromAPI reads the GetPlaybackResources response body and finds English .ttml2 URLs
func extractSubURLFromAPI(ctx context.Context, mon *monitorState) []string {
	mon.mu.Lock()
	ids := make([]network.RequestID, len(mon.apiReqIDs))
	copy(ids, mon.apiReqIDs)
	mon.mu.Unlock()

	if len(ids) == 0 {
		log.Println("  [!] No GetPlaybackResources API responses captured")
		return nil
	}

	ttml2Re := regexp.MustCompile(`https?://[^"\s]+\.ttml2[^"\s]*`)
	var foundURLs []string
	seenURLs := make(map[string]bool)

	for _, reqID := range ids {
		var body []byte
		err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var e error
			body, e = network.GetResponseBody(reqID).Do(ctx)
			return e
		}))
		if err != nil {
			log.Printf("  [!] Failed to read API response %s: %v", reqID, err)
			continue
		}
		log.Printf("  [>] API response body: %d bytes", len(body))
		os.WriteFile("api_response.json", body, 0644)

		// Parse the JSON and look for subtitle track entries
		var raw map[string]interface{}
		if err := json.Unmarshal(body, &raw); err == nil {
			// Try to find English subtitle URLs in the structured response
			jsonURLs := findEnglishSubInJSON(raw)
			for _, u := range jsonURLs {
				if !seenURLs[u] {
					seenURLs[u] = true
					foundURLs = append(foundURLs, u)
					log.Printf("  [OK] Found English subtitle (JSON parse): %s", truncate(u, 120))
				}
			}
			if len(foundURLs) > 0 {
				continue // Skip regex if JSON successfully found URLs
			}
		}

		// Fallback: regex all .ttml2 URLs and pick the best match
		matches := ttml2Re.FindAllString(string(body), -1)
		if len(matches) > 0 {
			log.Printf("  [>] Found %d .ttml2 URLs in API response", len(matches))

			// Look for English language indicators near the URL in the raw JSON
			bodyStr := string(body)
			for _, m := range matches {
				// Check surrounding context for English language markers
				idx := strings.Index(bodyStr, m)
				if idx > 0 {
					// Look at ~200 chars before the URL for language info
					start := idx - 200
					if start < 0 {
						start = 0
					}
					context := strings.ToLower(bodyStr[start:idx])
					if strings.Contains(context, `"en_gb"`) ||
						strings.Contains(context, `"en_us"`) ||
						strings.Contains(context, `"en"`) ||
						strings.Contains(context, `"english"`) ||
						strings.Contains(context, `"en-gb"`) ||
						strings.Contains(context, `"en-us"`) {
						if !seenURLs[m] {
							seenURLs[m] = true
							foundURLs = append(foundURLs, m)
							log.Printf("  [OK] Found English subtitle URL (context): %s", truncate(m, 120))
						}
					}
				}
			}
			
			if len(foundURLs) == 0 && len(matches) > 0 {
				log.Printf("  [!] No confirmed English track in regex context, taking last URL")
				lastURL := matches[len(matches)-1]
				if !seenURLs[lastURL] {
					seenURLs[lastURL] = true
					foundURLs = append(foundURLs, lastURL)
				}
			}
		}
	}
	return foundURLs
}

// findEnglishSubInJSON searches the Amazon API response for English subtitle URLs.
// Prefers SDH/CC tracks (contain speaker labels) over plain subtitles when preferSDH is set.
func findEnglishSubInJSON(data interface{}) []string {
	root, ok := data.(map[string]interface{})
	if !ok {
		return nil
	}

	// --- Content validation: warn if this looks like a trailer ---
	if meta, ok := root["catalogMetadata"].(map[string]interface{}); ok {
		if cat, ok := meta["catalog"].(map[string]interface{}); ok {
			if rt, ok := cat["runtimeSeconds"].(float64); ok && rt < float64(minRuntimeSecs) {
				log.Printf("  [!] WARNING: Content runtime is only %.0fs (<%ds) — may be a trailer, not an episode!", rt, minRuntimeSecs)
				if title, ok := cat["title"].(string); ok {
					log.Printf("  [!] Content title: %q", title)
				}
				if etype, ok := cat["entityType"].(string); ok {
					log.Printf("  [!] Content type: %q", etype)
				}
			}
		}
	}

	// --- Strategy 1: Parse the structured subtitleUrls array (most reliable) ---
	if subURLs, ok := root["subtitleUrls"].([]interface{}); ok && len(subURLs) > 0 {
		var sdhURL, plainURL string
		for _, item := range subURLs {
			track, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			lang, _ := track["languageCode"].(string)
			if !strings.HasPrefix(strings.ToLower(lang), "en") {
				continue
			}
			url, _ := track["url"].(string)
			if url == "" || !strings.Contains(url, ".ttml2") {
				continue
			}
			trackType, _ := track["type"].(string) // "sdh" or "subtitle"
			displayName, _ := track["displayName"].(string)

			if trackType == "sdh" || strings.Contains(strings.ToLower(displayName), "[cc]") {
				sdhURL = url
				log.Printf("  [OK] Found English SDH/CC track: %s", truncate(url, 120))
			} else if plainURL == "" {
				plainURL = url
				log.Printf("  [OK] Found English subtitle track: %s", truncate(url, 120))
			}
		}
		// Prefer SDH when configured (has speaker labels like [Homelander])
		if preferSDH && sdhURL != "" {
			log.Println("  [>] Using SDH track (includes speaker labels and sound descriptions)")
			return []string{sdhURL}
		}
		if plainURL != "" {
			return []string{plainURL}
		}
		if sdhURL != "" {
			return []string{sdhURL}
		}
	}

	// --- Strategy 2: Recursive generic search (fallback for non-standard responses) ---
	return findSubURLsRecursive(data)
}

// findSubURLsRecursive does a deep search for English .ttml2 URLs in arbitrary JSON
func findSubURLsRecursive(data interface{}) []string {
	var urls []string
	switch v := data.(type) {
	case map[string]interface{}:
		lang, _ := v["languageCode"].(string)
		if lang == "" {
			lang, _ = v["language"].(string)
		}
		if lang == "" {
			lang, _ = v["locale"].(string)
		}
		isEnglish := strings.HasPrefix(strings.ToLower(lang), "en")

		url, _ := v["url"].(string)
		if isEnglish && url != "" && strings.Contains(url, ".ttml2") {
			urls = append(urls, url)
		}

		for _, val := range v {
			urls = append(urls, findSubURLsRecursive(val)...)
		}
	case []interface{}:
		for _, item := range v {
			urls = append(urls, findSubURLsRecursive(item)...)
		}
	}
	return urls
}

// downloadSubtitleHTTP fetches a subtitle file directly via HTTP GET
func downloadSubtitleHTTP(url string) []byte {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.Fatalf("[X] HTTP download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("[X] HTTP download status %d for %s", resp.StatusCode, truncate(url, 100))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("[X] Failed to read HTTP response body: %v", err)
	}
	log.Printf("  [OK] Downloaded %d bytes via HTTP", len(body))
	return body
}

func downloadBody(ctx context.Context, sub *capturedSub) []byte {
	var body []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var e error
		body, e = network.GetResponseBody(sub.requestID).Do(ctx)
		return e
	}))
	if err != nil {
		log.Fatalf("[X] Failed to download response body: %v", err)
	}
	return body
}

// ==========================================================================
//  TTML2 PARSER - preserves timestamps + speaker labels for LLM analysis
// ==========================================================================

func parseTTML2(data []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	b.Grow(len(data) / 2)
	depth := 0
	var currentBegin string

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				depth++
				// Extract begin timestamp for chronological tracking
				for _, attr := range t.Attr {
					if attr.Name.Local == "begin" {
						currentBegin = formatTimestamp(attr.Value)
						break
					}
				}
				if currentBegin != "" {
					b.WriteString("[")
					b.WriteString(currentBegin)
					b.WriteString("] ")
				}
			case "br":
				if depth > 0 {
					b.WriteByte(' ')
				}
			}
		case xml.EndElement:
			if t.Name.Local == "p" && depth > 0 {
				depth--
				currentBegin = ""
				b.WriteByte('\n')
			}
		case xml.CharData:
			if depth > 0 {
				text := bytes.TrimSpace(t)
				if len(text) > 0 {
					b.Write(text)
					b.WriteByte(' ')
				}
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// formatTimestamp converts "00:01:23.456" -> "00:01:23" (drop millis for readability)
func formatTimestamp(ts string) string {
	if idx := strings.IndexByte(ts, '.'); idx > 0 {
		return ts[:idx]
	}
	return ts
}

// ==========================================================================
//  GEMINI API - direct HTTP, free tier
// ==========================================================================

type geminiRequest struct {
	Contents         []geminiContent         `json:"contents"`
	GenerationConfig *geminiGenerationConfig `json:"generationConfig,omitempty"`
}
type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}
type geminiPart struct {
	Text string `json:"text"`
}
type geminiGenerationConfig struct {
	ThinkingConfig *thinkingConfig `json:"thinkingConfig,omitempty"`
	MaxOutputTokens int           `json:"maxOutputTokens,omitempty"`
}
type thinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought,omitempty"` // thinking parts are internal reasoning
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error,omitempty"`
}

var httpTransport = &http.Transport{
	MaxIdleConns:        2,
	MaxIdleConnsPerHost: 2,
	IdleConnTimeout:     90 * time.Second,
	ForceAttemptHTTP2:   true,
}

func callGemini(apiKey, content string) string {
	reqBody := geminiRequest{
		Contents: []geminiContent{{
			Parts: []geminiPart{{Text: content}},
		}},
		GenerationConfig: &geminiGenerationConfig{
			ThinkingConfig:  &thinkingConfig{ThinkingBudget: 10000},
			MaxOutputTokens: 65536,
		},
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		log.Fatalf("[X] Marshal: %v", err)
	}

	apiURL := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		geminiModel, apiKey,
	)
	client := &http.Client{Transport: httpTransport, Timeout: apiTimeout}

	const maxRetries = 4
	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest("POST", apiURL, bytes.NewReader(jsonData))
		if err != nil {
			log.Fatalf("[X] Create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Fatalf("[X] Gemini request failed: %v", err)
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			wait := time.Duration(30*attempt) * time.Second
			log.Printf("  [!] Rate limited (429), retry %d/%d in %s...", attempt, maxRetries, wait)
			time.Sleep(wait)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Fatalf("[X] Gemini API error %d: %s", resp.StatusCode, string(bodyBytes))
		}

		var gemResp geminiResponse
		if err := json.NewDecoder(resp.Body).Decode(&gemResp); err != nil {
			resp.Body.Close()
			log.Fatalf("[X] Decode response: %v", err)
		}
		resp.Body.Close()

		if gemResp.Error != nil {
			log.Fatalf("[X] Gemini error %d: %s", gemResp.Error.Code, gemResp.Error.Message)
		}
		if len(gemResp.Candidates) == 0 || len(gemResp.Candidates[0].Content.Parts) == 0 {
			log.Fatal("[X] Gemini returned no content")
		}

		var sb strings.Builder
		for _, p := range gemResp.Candidates[0].Content.Parts {
			if !p.Thought { // skip internal thinking/reasoning parts
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	log.Fatal("[X] Gemini rate limit: all retries exhausted")
	return ""
}

// ==========================================================================
//  UTILITIES
// ==========================================================================

func resolveAPIKey() string {
	key := hardcodedAPIKey
	if key == "" {
		key = os.Getenv("GEMINI_API_KEY")
	}
	if key == "" {
		log.Fatal("[X] No Gemini API key. Set the GEMINI_API_KEY env var")
	}
	return key
}

func mustRun(ctx context.Context, actions ...chromedp.Action) {
	if err := chromedp.Run(ctx, actions...); err != nil {
		log.Fatalf("[X] Chrome action failed: %v", err)
	}
}

func writeFile(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		log.Fatalf("[X] Failed to write %s: %v", path, err)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func copyFile(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	os.WriteFile(dst, data, 0644)
}
