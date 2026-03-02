package viz

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/maxghenis/openmessage/internal/story"
)

// buildTemplate creates the parsed HTML template.
func buildTemplate() (*template.Template, error) {
	funcMap := template.FuncMap{
		"isChapter":   func(s section) bool { return s.Type == "chapter" },
		"asChapter":   func(d any) story.Chapter { return d.(story.Chapter) },
		"asString":    func(d any) string { return d.(string) },
		"formatHour":  formatHour,
		"dayName":     dayName,
		"phraseSize":  phraseSize,
		"phraseColor": phraseColor,
		"noescape":    func(s string) template.HTML { return template.HTML(s) },
		"noescapeJS":  func(s string) template.JS { return template.JS(s) },
		"noescapeCSS": func(s string) template.CSS { return template.CSS(s) },
		"seq24":       func() []int { return makeSeq(24) },
		"seq7":        func() []int { return makeSeq(7) },
		"mul":         func(a, b int) int { return a * b },
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"div":         func(a, b float64) float64 { if b == 0 { return 0 }; return a / b },
	}

	return template.New("viz").Funcs(funcMap).Parse(htmlTemplate)
}

func makeSeq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

func formatHour(h int) string {
	if h == 0 {
		return "12a"
	}
	if h < 12 {
		return fmt.Sprintf("%da", h)
	}
	if h == 12 {
		return "12p"
	}
	return fmt.Sprintf("%dp", h-12)
}

func dayName(d int) string {
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	if d >= 0 && d < 7 {
		return names[d]
	}
	return ""
}

// phraseSize returns a CSS font-size for a phrase based on its count relative to max.
func phraseSize(count, maxCount int) string {
	if maxCount == 0 {
		return "1rem"
	}
	// Scale from 0.8rem to 3rem
	ratio := float64(count) / float64(maxCount)
	size := 0.8 + ratio*2.2
	return fmt.Sprintf("%.1frem", size)
}

// phraseColor returns a CSS color interpolated between primary and secondary based on BySender.
// Primary represents "me" (person1), secondary represents others (person2).
// The color blends based on the ratio of non-"me" usage.
func phraseColor(bySender map[string]int, primary, secondary string) string {
	if len(bySender) == 0 {
		return primary
	}
	total := 0
	for _, c := range bySender {
		total += c
	}
	if total == 0 {
		return primary
	}

	// Count messages from non-"me" senders to determine blend ratio
	meCount := bySender["me"]
	otherCount := total - meCount

	ratio := float64(otherCount) / float64(total)

	// Parse hex colors and interpolate
	r1, g1, b1 := parseHex(primary)
	r2, g2, b2 := parseHex(secondary)

	r := int(float64(r1)*(1-ratio) + float64(r2)*ratio)
	g := int(float64(g1)*(1-ratio) + float64(g2)*ratio)
	b := int(float64(b1)*(1-ratio) + float64(b2)*ratio)

	return fmt.Sprintf("rgb(%d, %d, %d)", r, g, b)
}

func parseHex(hex string) (r, g, b int) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	if len(hex) != 6 {
		return 200, 200, 200
	}
	fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Story.Title}} — {{.Config.Person1}} & {{.Config.Person2}}</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Cormorant+Garamond:ital,wght@0,300;0,400;0,600;1,300;1,400&family=DM+Sans:wght@300;400;500;700&display=swap" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
<style>
/* ============ Design System ============ */
:root {
    --primary: {{.Config.PrimaryColor}};
    --secondary: {{.Config.SecondaryColor}};
    --accent: {{.Config.AccentColor}};
    --bg: {{.Config.BackgroundColor}};
    --text: #fafaf9;
    --text-muted: #a8a29e;
    --font-serif: 'Cormorant Garamond', Georgia, serif;
    --font-sans: 'DM Sans', -apple-system, sans-serif;
    --gutter: clamp(1rem, 4vw, 3rem);
}

*,
*::before,
*::after {
    margin: 0;
    padding: 0;
    box-sizing: border-box;
}

html {
    font-size: 16px;
    scroll-behavior: smooth;
    -webkit-font-smoothing: antialiased;
}

body {
    font-family: var(--font-sans);
    background: var(--bg);
    color: var(--text);
    line-height: 1.6;
    overflow-x: hidden;
    min-height: 100vh;
}

/* Grain overlay */
body::after {
    content: '';
    position: fixed;
    inset: 0;
    pointer-events: none;
    z-index: 9999;
    opacity: 0.035;
    background-image: url("data:image/svg+xml,%3Csvg viewBox='0 0 256 256' xmlns='http://www.w3.org/2000/svg'%3E%3Cfilter id='grain'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='4' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23grain)'/%3E%3C/svg%3E");
    background-repeat: repeat;
    background-size: 256px 256px;
}

/* ============ Typography ============ */
h1, h2, h3 {
    font-family: var(--font-serif);
    font-weight: 300;
    letter-spacing: -0.02em;
}

h1 { font-size: clamp(2.5rem, 6vw, 5rem); line-height: 1.1; }
h2 { font-size: clamp(1.8rem, 4vw, 3rem); line-height: 1.2; }
h3 { font-size: clamp(1.3rem, 2.5vw, 1.8rem); line-height: 1.3; }

/* ============ Layout ============ */
.container {
    max-width: 900px;
    margin: 0 auto;
    padding: 0 var(--gutter);
}

.section {
    padding: 6rem 0;
    position: relative;
}

.section + .section {
    border-top: 1px solid rgba(255,255,255,0.04);
}

/* ============ Scroll Reveal ============ */
.reveal {
    opacity: 0;
    transform: translateY(40px);
    transition: opacity 0.8s cubic-bezier(0.16, 1, 0.3, 1),
                transform 0.8s cubic-bezier(0.16, 1, 0.3, 1);
}

.reveal.visible {
    opacity: 1;
    transform: translateY(0);
}

/* ============ Password Gate ============ */
{{if .Config.PasswordHash}}
.password-overlay {
    position: fixed;
    inset: 0;
    z-index: 10000;
    background: var(--bg);
    display: flex;
    align-items: center;
    justify-content: center;
    flex-direction: column;
    gap: 1.5rem;
    transition: opacity 0.6s ease, visibility 0.6s ease;
}

.password-overlay.hidden {
    opacity: 0;
    visibility: hidden;
    pointer-events: none;
}

.password-overlay h2 {
    color: var(--text-muted);
    font-weight: 300;
}

.password-overlay input {
    background: rgba(255,255,255,0.06);
    border: 1px solid rgba(255,255,255,0.12);
    border-radius: 8px;
    padding: 0.8rem 1.2rem;
    font-size: 1.1rem;
    color: var(--text);
    font-family: var(--font-sans);
    width: 280px;
    text-align: center;
    outline: none;
    transition: border-color 0.3s;
}

.password-overlay input:focus {
    border-color: var(--primary);
}

.password-overlay .error {
    color: var(--primary);
    font-size: 0.85rem;
    opacity: 0;
    transition: opacity 0.3s;
}

.password-overlay .error.show {
    opacity: 1;
}

.password-overlay button {
    background: var(--primary);
    color: white;
    border: none;
    border-radius: 8px;
    padding: 0.7rem 2rem;
    font-size: 1rem;
    font-family: var(--font-sans);
    cursor: pointer;
    transition: transform 0.2s, opacity 0.2s;
}

.password-overlay button:hover {
    transform: scale(1.03);
}
{{end}}

/* ============ Hero ============ */
.hero {
    min-height: 100vh;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    text-align: center;
    padding: 4rem var(--gutter);
    position: relative;
}

.hero::before {
    content: '';
    position: absolute;
    top: 0;
    left: 50%;
    transform: translateX(-50%);
    width: 600px;
    height: 600px;
    border-radius: 50%;
    background: radial-gradient(circle, var(--primary) 0%, transparent 70%);
    opacity: 0.06;
    pointer-events: none;
}

.hero-names {
    font-family: var(--font-serif);
    font-size: clamp(3rem, 8vw, 6.5rem);
    font-weight: 300;
    line-height: 1.05;
    margin-bottom: 1.5rem;
}

.hero-names .ampersand {
    display: block;
    font-size: 0.5em;
    color: var(--accent);
    font-style: italic;
    line-height: 1.6;
}

.hero-subtitle {
    font-family: var(--font-serif);
    font-style: italic;
    font-size: clamp(1.1rem, 2.5vw, 1.5rem);
    color: var(--text-muted);
    margin-bottom: 3rem;
    max-width: 500px;
}

.hero-stats {
    display: flex;
    gap: 3rem;
    flex-wrap: wrap;
    justify-content: center;
}

.hero-stat {
    text-align: center;
}

.hero-stat .number {
    font-family: var(--font-serif);
    font-size: clamp(2rem, 4vw, 3rem);
    font-weight: 600;
    color: var(--accent);
    display: block;
}

.hero-stat .label {
    font-size: 0.8rem;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.15em;
}

/* ============ Chapters ============ */
.chapter {
    padding: 5rem 0;
}

.chapter-period {
    font-size: 0.75rem;
    color: var(--accent);
    text-transform: uppercase;
    letter-spacing: 0.2em;
    margin-bottom: 0.5rem;
}

.chapter h2 {
    margin-bottom: 1.5rem;
    color: var(--text);
}

.chapter-content {
    font-size: 1.05rem;
    line-height: 1.8;
    color: var(--text-muted);
    margin-bottom: 2rem;
}

/* Chat bubbles */
.chat-bubbles {
    display: flex;
    flex-direction: column;
    gap: 0.8rem;
    max-width: 600px;
    margin: 2rem auto;
}

.bubble {
    max-width: 80%;
    padding: 0.75rem 1rem;
    border-radius: 18px;
    font-size: 0.95rem;
    line-height: 1.5;
    position: relative;
    animation: bubbleFade 0.6s ease both;
}

.bubble-left {
    align-self: flex-start;
    background: rgba(255,255,255,0.08);
    border-bottom-left-radius: 4px;
    color: var(--text);
}

.bubble-right {
    align-self: flex-end;
    background: var(--primary);
    border-bottom-right-radius: 4px;
    color: white;
}

.bubble-sender {
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.1em;
    opacity: 0.6;
    margin-bottom: 0.2rem;
}

.bubble-time {
    font-size: 0.65rem;
    opacity: 0.5;
    margin-top: 0.3rem;
    text-align: right;
}

@keyframes bubbleFade {
    from { opacity: 0; transform: translateY(10px); }
    to { opacity: 1; transform: translateY(0); }
}

/* ============ Volume Chart ============ */
.chart-wrapper {
    position: relative;
    width: 100%;
    max-height: 400px;
}

.chart-wrapper canvas {
    width: 100% !important;
}

/* ============ Sender Split ============ */
.donut-wrapper {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 3rem;
    flex-wrap: wrap;
}

.donut-chart {
    width: 220px;
    height: 220px;
}

.donut-legend {
    display: flex;
    flex-direction: column;
    gap: 1rem;
}

.legend-item {
    display: flex;
    align-items: center;
    gap: 0.8rem;
}

.legend-swatch {
    width: 14px;
    height: 14px;
    border-radius: 3px;
}

.legend-label {
    font-size: 0.95rem;
    color: var(--text-muted);
}

.legend-value {
    font-family: var(--font-serif);
    font-size: 1.5rem;
    font-weight: 600;
    color: var(--text);
}

/* ============ Response Times ============ */
.response-bars {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
    margin-top: 2rem;
}

.response-bar {
    display: flex;
    align-items: center;
    gap: 1rem;
}

.response-bar .name {
    width: 80px;
    text-align: right;
    font-size: 0.85rem;
    color: var(--text-muted);
}

.response-bar .bar-track {
    flex: 1;
    height: 8px;
    background: rgba(255,255,255,0.06);
    border-radius: 4px;
    overflow: hidden;
}

.response-bar .bar-fill {
    height: 100%;
    border-radius: 4px;
    transition: width 1s cubic-bezier(0.16, 1, 0.3, 1);
}

.response-bar .time {
    width: 80px;
    font-size: 0.85rem;
    color: var(--text-muted);
}

/* ============ Heatmap ============ */
.heatmap-grid {
    display: grid;
    grid-template-columns: 50px repeat(7, 1fr);
    grid-template-rows: auto repeat(24, 1fr);
    gap: 2px;
    max-width: 600px;
    margin: 2rem auto;
}

.heatmap-header {
    text-align: center;
    font-size: 0.7rem;
    color: var(--text-muted);
    text-transform: uppercase;
    letter-spacing: 0.1em;
    padding: 0.3rem;
}

.heatmap-hour {
    text-align: right;
    font-size: 0.65rem;
    color: var(--text-muted);
    padding: 0.2rem 0.5rem;
    display: flex;
    align-items: center;
    justify-content: flex-end;
}

.heatmap-cell {
    aspect-ratio: 1;
    border-radius: 3px;
    min-height: 16px;
    transition: transform 0.2s;
    cursor: default;
    position: relative;
}

.heatmap-cell:hover {
    transform: scale(1.3);
    z-index: 1;
}

.heatmap-cell[title]:hover::after {
    content: attr(title);
    position: absolute;
    bottom: calc(100% + 4px);
    left: 50%;
    transform: translateX(-50%);
    background: rgba(0,0,0,0.85);
    color: var(--text);
    padding: 0.2rem 0.5rem;
    border-radius: 4px;
    font-size: 0.65rem;
    white-space: nowrap;
    pointer-events: none;
}

/* ============ Phrase Cloud ============ */
.phrase-cloud {
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    align-items: baseline;
    gap: 0.6rem 1.2rem;
    padding: 2rem 0;
}

.phrase {
    font-family: var(--font-serif);
    font-weight: 400;
    cursor: default;
    transition: transform 0.2s, opacity 0.2s;
    position: relative;
    line-height: 1.3;
}

.phrase:hover {
    transform: scale(1.1);
}

.phrase .tooltip {
    display: none;
    position: absolute;
    bottom: calc(100% + 8px);
    left: 50%;
    transform: translateX(-50%);
    background: rgba(0,0,0,0.9);
    color: var(--text);
    padding: 0.4rem 0.8rem;
    border-radius: 6px;
    font-family: var(--font-sans);
    font-size: 0.75rem;
    white-space: nowrap;
    z-index: 10;
}

.phrase:hover .tooltip {
    display: block;
}

/* ============ Longest Gap / Silence ============ */
.silence-card {
    background: rgba(255,255,255,0.03);
    border: 1px solid rgba(255,255,255,0.06);
    border-radius: 16px;
    padding: 3rem;
    text-align: center;
    max-width: 500px;
    margin: 0 auto;
}

.silence-card .days {
    font-family: var(--font-serif);
    font-size: clamp(3rem, 6vw, 5rem);
    font-weight: 300;
    color: var(--accent);
    line-height: 1;
}

.silence-card .days-label {
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.2em;
    color: var(--text-muted);
    margin-top: 0.3rem;
}

.silence-card .gap-dates {
    margin-top: 1rem;
    font-size: 0.85rem;
    color: var(--text-muted);
    font-style: italic;
}

/* ============ Interludes ============ */
.interlude {
    text-align: center;
    padding: 4rem var(--gutter);
    max-width: 600px;
    margin: 0 auto;
}

.interlude-text {
    font-family: var(--font-serif);
    font-style: italic;
    font-size: clamp(1.2rem, 2.5vw, 1.6rem);
    color: var(--text-muted);
    line-height: 1.8;
}

/* ============ Photos ============ */
.photo-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
    gap: 0.5rem;
}

.photo-grid img {
    width: 100%;
    height: 200px;
    object-fit: cover;
    border-radius: 8px;
    transition: transform 0.3s;
}

.photo-grid img:hover {
    transform: scale(1.05);
}

/* ============ Closing ============ */
.closing {
    text-align: center;
    padding: 8rem var(--gutter);
}

.closing h2 {
    font-size: clamp(1.5rem, 3vw, 2.5rem);
    color: var(--text-muted);
    font-weight: 300;
}

.closing .still-writing {
    margin-top: 1rem;
    font-size: 0.85rem;
    color: var(--accent);
    font-style: italic;
}

/* ============ Responsive ============ */
@media (max-width: 640px) {
    .hero-stats {
        gap: 1.5rem;
    }
    .donut-wrapper {
        flex-direction: column;
    }
    .heatmap-grid {
        grid-template-columns: 40px repeat(7, 1fr);
    }
}
</style>
</head>
<body>

{{if .Config.PasswordHash}}
<!-- Password Gate -->
<div class="password-overlay" id="password-gate">
    <h2>Enter the password</h2>
    <input type="password" id="pw-input" placeholder="Password" autocomplete="off" autofocus>
    <div class="error" id="pw-error">Incorrect password</div>
    <button onclick="checkPassword()">Unlock</button>
</div>
<script>
(function() {
    var expectedHash = '{{.Config.PasswordHash}}';

    window.checkPassword = async function() {
        var input = document.getElementById('pw-input').value;
        var encoder = new TextEncoder();
        var data = encoder.encode(input);
        var hashBuf = await crypto.subtle.digest('SHA-256', data);
        var hashArr = Array.from(new Uint8Array(hashBuf));
        var hashHex = hashArr.map(function(b) { return b.toString(16).padStart(2, '0'); }).join('');

        if (hashHex === expectedHash) {
            document.getElementById('password-gate').classList.add('hidden');
        } else {
            document.getElementById('pw-error').classList.add('show');
            document.getElementById('pw-input').value = '';
            setTimeout(function() {
                document.getElementById('pw-error').classList.remove('show');
            }, 2000);
        }
    };

    document.addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && document.getElementById('password-gate') &&
            !document.getElementById('password-gate').classList.contains('hidden')) {
            checkPassword();
        }
    });
})();
</script>
{{end}}

<!-- Main Content -->
<main id="main-content">
{{range $i, $sec := .Sections}}
{{if eq $sec.Type "hero"}}
<!-- ==================== HERO ==================== -->
<section class="hero section reveal" id="section-hero">
    <div class="hero-names">
        {{$.Config.Person1}}<span class="ampersand">&amp;</span>{{$.Config.Person2}}
    </div>
    {{if $.Story}}
    <div class="hero-subtitle">{{$.Story.Summary}}</div>
    {{end}}
    <div class="hero-stats">
        <div class="hero-stat">
            <span class="number">{{$.Stats.TotalMessages}}</span>
            <span class="label">Messages</span>
        </div>
        <div class="hero-stat">
            <span class="number">{{$.DaysSpan}}</span>
            <span class="label">Days</span>
        </div>
        <div class="hero-stat">
            <span class="number">{{$.MessagesPerDay}}</span>
            <span class="label">Per day</span>
        </div>
    </div>
</section>

{{else if eq $sec.Type "chapter"}}
<!-- ==================== CHAPTER ==================== -->
{{$ch := asChapter $sec.Data}}
<section class="chapter section reveal" id="section-chapter-{{$i}}">
    <div class="container">
        <div class="chapter-period">{{$ch.Period}}</div>
        <h2>{{$ch.Title}}</h2>
        <div class="chapter-content">{{$ch.Content}}</div>
        {{if $ch.Quotes}}
        <div class="chat-bubbles">
            {{range $ch.Quotes}}
            <div class="bubble {{if eq .Sender $.Config.Person1}}bubble-right{{else}}bubble-left{{end}}">
                <div class="bubble-sender">{{.Sender}}</div>
                <div>{{.Text}}</div>
                {{if .Timestamp}}<div class="bubble-time">{{.Timestamp}}</div>{{end}}
            </div>
            {{end}}
        </div>
        {{end}}
    </div>
</section>

{{else if eq $sec.Type "volume-chart"}}
<!-- ==================== VOLUME CHART ==================== -->
<section class="section reveal" id="section-volume-chart">
    <div class="container">
        <h2>Messages over time</h2>
        <div class="chart-wrapper" style="margin-top: 2rem;">
            <canvas id="volumeChart"></canvas>
        </div>
    </div>
</section>

{{else if eq $sec.Type "sender-split"}}
<!-- ==================== SENDER SPLIT ==================== -->
<section class="section reveal" id="section-sender-split">
    <div class="container">
        <h2>Who texts more?</h2>
        <div class="donut-wrapper" style="margin-top: 2rem;">
            <div class="donut-chart">
                <canvas id="donutChart"></canvas>
            </div>
            <div class="donut-legend">
                {{range $sender, $count := $.Stats.SenderSplit}}
                <div class="legend-item">
                    <div class="legend-swatch" style="background: {{if eq $sender $.Config.Person1}}{{$.Config.PrimaryColor}}{{else}}{{$.Config.SecondaryColor}}{{end}};"></div>
                    <div>
                        <div class="legend-label">{{$sender}}</div>
                        <div class="legend-value">{{$count}}</div>
                    </div>
                </div>
                {{end}}
            </div>
        </div>
        {{if $.Stats.AvgResponseTimes}}
        <h3 style="margin-top: 3rem;">Average response time</h3>
        <div class="response-bars">
            {{range $sender, $minutes := $.Stats.AvgResponseTimes}}
            <div class="response-bar">
                <span class="name">{{$sender}}</span>
                <div class="bar-track">
                    <div class="bar-fill" style="width: 0%; background: {{if eq $sender $.Config.Person1}}{{$.Config.PrimaryColor}}{{else}}{{$.Config.SecondaryColor}}{{end}};" data-width="{{$minutes}}"></div>
                </div>
                <span class="time">{{$minutes}} min</span>
            </div>
            {{end}}
        </div>
        {{end}}
    </div>
</section>

{{else if eq $sec.Type "heatmap"}}
<!-- ==================== HEATMAP ==================== -->
<section class="section reveal" id="section-heatmap">
    <div class="container">
        <h2>When do you talk?</h2>
        <div class="heatmap-grid" style="margin-top: 2rem;">
            <!-- Header row -->
            <div class="heatmap-header"></div>
            {{range seq7}}
            <div class="heatmap-header">{{dayName .}}</div>
            {{end}}
            <!-- Data rows (24 hours) -->
            {{range $h := seq24}}
            <div class="heatmap-hour">{{formatHour $h}}</div>
            {{range $d := seq7}}
            <div class="heatmap-cell" id="hm-{{$h}}-{{$d}}"></div>
            {{end}}
            {{end}}
        </div>
    </div>
</section>

{{else if eq $sec.Type "phrases"}}
<!-- ==================== PHRASE CLOUD ==================== -->
<section class="section reveal" id="section-phrases">
    <div class="container">
        <h2>Your words</h2>
        <div class="phrase-cloud">
            {{$maxCount := 0}}
            {{range $.Stats.TopPhrases}}{{if gt .Count $maxCount}}{{$maxCount = .Count}}{{end}}{{end}}
            {{range $.Stats.TopPhrases}}
            <span class="phrase"
                style="font-size: {{phraseSize .Count $maxCount}}; color: {{phraseColor .BySender $.Config.PrimaryColor $.Config.SecondaryColor}};">
                {{.Phrase}}
                <span class="tooltip">
                    {{.Phrase}}: {{.Count}} times
                    {{range $sender, $count := .BySender}}
                    <br>{{$sender}}: {{$count}}
                    {{end}}
                </span>
            </span>
            {{end}}
        </div>
    </div>
</section>

{{else if eq $sec.Type "silence"}}
<!-- ==================== LONGEST GAP ==================== -->
{{if gt $.Stats.LongestGap.Days 0}}
<section class="section reveal" id="section-silence">
    <div class="container">
        <h2>The longest silence</h2>
        <div class="silence-card" style="margin-top: 2rem;">
            <div class="days">{{$.Stats.LongestGap.Days}}</div>
            <div class="days-label">days of silence</div>
            <div class="gap-dates">{{$.Stats.LongestGap.Start}} &mdash; {{$.Stats.LongestGap.End}}</div>
        </div>
    </div>
</section>
{{end}}

{{else if eq $sec.Type "photos"}}
<!-- ==================== PHOTOS ==================== -->
{{if $.Config.PhotoPaths}}
<section class="section reveal" id="section-photos">
    <div class="container">
        <h2>Moments</h2>
        <div class="photo-grid" style="margin-top: 2rem;">
            {{range $.Config.PhotoPaths}}
            <img src="{{.}}" alt="Photo" loading="lazy">
            {{end}}
        </div>
    </div>
</section>
{{end}}

{{else if eq $sec.Type "closing"}}
<!-- ==================== CLOSING ==================== -->
<section class="closing section reveal" id="section-closing">
    <h2>{{$.Stats.DateRange.Start}} &mdash; {{$.Stats.DateRange.End}}</h2>
    <div class="still-writing">still writing...</div>
</section>

{{else if eq $sec.Type "interlude"}}
<!-- ==================== INTERLUDE ==================== -->
<div class="interlude reveal">
    <div class="interlude-text">{{asString $sec.Data}}</div>
</div>

{{else if eq $sec.Type "timeline-nav"}}
<!-- ==================== TIMELINE NAV ==================== -->
{{if $.Story}}
<nav class="section" id="section-timeline-nav" style="text-align: center;">
    <div class="container">
        {{range $ci, $ch := $.Story.Chapters}}
        <a href="#" style="color: var(--text-muted); text-decoration: none; margin: 0 1rem; font-size: 0.85rem; text-transform: uppercase; letter-spacing: 0.1em;">{{$ch.Period}}</a>
        {{end}}
    </div>
</nav>
{{end}}

{{end}}
{{end}}
</main>

<!-- ==================== SCRIPTS ==================== -->
<script>
(function() {
    'use strict';

    // ---- Scroll Reveal via IntersectionObserver ----
    var observer = new IntersectionObserver(function(entries) {
        entries.forEach(function(entry) {
            if (entry.isIntersecting) {
                entry.target.classList.add('visible');
            }
        });
    }, { threshold: 0.1, rootMargin: '0px 0px -40px 0px' });

    document.querySelectorAll('.reveal').forEach(function(el) {
        observer.observe(el);
    });

    // ---- Chart.js Global Config ----
    Chart.defaults.color = '#a8a29e';
    Chart.defaults.font.family = "'DM Sans', sans-serif";
    Chart.defaults.plugins.legend.display = false;

    // ---- Volume Chart (Stacked Bar) ----
    var volumeEl = document.getElementById('volumeChart');
    if (volumeEl) {
        var vCtx = volumeEl.getContext('2d');
        new Chart(vCtx, {
            type: 'bar',
            data: {
                labels: {{noescapeJS .MonthlyLabelsJSON}},
                datasets: [
                    {
                        label: '{{.Config.Person1}}',
                        data: {{noescapeJS .MonthlySeries1JSON}},
                        backgroundColor: '{{.Config.PrimaryColor}}',
                        borderRadius: 2,
                    },
                    {
                        label: '{{.Config.Person2}}',
                        data: {{noescapeJS .MonthlySeries2JSON}},
                        backgroundColor: '{{.Config.SecondaryColor}}',
                        borderRadius: 2,
                    }
                ]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                scales: {
                    x: {
                        stacked: true,
                        grid: { display: false },
                        ticks: {
                            maxRotation: 45,
                            autoSkip: true,
                            maxTicksLimit: 24,
                            font: { size: 10 }
                        }
                    },
                    y: {
                        stacked: true,
                        grid: { color: 'rgba(255,255,255,0.04)' },
                        ticks: { font: { size: 10 } }
                    }
                },
                plugins: {
                    legend: { display: true, position: 'top', labels: { boxWidth: 12, padding: 20 } },
                    tooltip: {
                        backgroundColor: 'rgba(0,0,0,0.8)',
                        titleFont: { size: 12 },
                        bodyFont: { size: 11 },
                    }
                }
            }
        });
    }

    // ---- Donut Chart ----
    var donutEl = document.getElementById('donutChart');
    if (donutEl) {
        var dCtx = donutEl.getContext('2d');
        new Chart(dCtx, {
            type: 'doughnut',
            data: {
                labels: {{noescapeJS .SenderLabelsJSON}},
                datasets: [{
                    data: {{noescapeJS .SenderValuesJSON}},
                    backgroundColor: ['{{.Config.PrimaryColor}}', '{{.Config.SecondaryColor}}'],
                    borderWidth: 0,
                    borderRadius: 4,
                    spacing: 4,
                }]
            },
            options: {
                responsive: true,
                cutout: '70%',
                plugins: {
                    legend: { display: false }
                }
            }
        });
    }

    // ---- Heatmap Coloring ----
    var heatmapData = {{noescapeJS .HeatmapJSON}};
    var primaryColor = '{{.Config.PrimaryColor}}';
    heatmapData.forEach(function(cell) {
        var el = document.getElementById('hm-' + cell.Hour + '-' + cell.Day);
        if (el) {
            if (cell.Count > 0) {
                el.style.backgroundColor = primaryColor;
                el.style.opacity = cell.Opacity;
            } else {
                el.style.backgroundColor = 'rgba(255,255,255,0.03)';
            }
            el.title = cell.Count + ' messages';
        }
    });

    // ---- Response Bar Animation ----
    document.querySelectorAll('.response-bar .bar-fill').forEach(function(bar) {
        var width = parseInt(bar.dataset.width, 10);
        // Scale to max 100%: find max among siblings
        var maxWidth = 0;
        document.querySelectorAll('.response-bar .bar-fill').forEach(function(b) {
            var w = parseInt(b.dataset.width, 10);
            if (w > maxWidth) maxWidth = w;
        });
        var pct = maxWidth > 0 ? Math.round((width / maxWidth) * 100) : 0;
        setTimeout(function() {
            bar.style.width = pct + '%';
        }, 500);
    });
})();
</script>
</body>
</html>`
