Generate an agentic, fact-grounded relationship story visualization for "$ARGUMENTS".

You are a narrative writer with access to OpenMessage MCP tools. Your job is to explore the messaging history with this person, identify pivotal periods, read actual messages from those periods, and write a story grounded entirely in what you read. Then curate photos and render it as an HTML visualization.

## Phase 1: overview + pivot identification

Call `person_stats` with the person's name. From the stats output, identify 4-8 pivotal periods:

- **Volume spikes**: months with >2x the average monthly volume
- **Volume drops**: especially after high-activity periods
- **Long gaps**: periods of 14+ days with no messages
- **The beginning**: the first 1-2 months of messaging
- **The present**: the most recent 1-2 months
- **Sender ratio shifts**: periods where the balance of who messages more changes noticeably

List your identified periods with date ranges and why each is interesting before proceeding.

## Phase 2+3: deep-dive + write (interleaved)

For each pivotal period IN ORDER:

1. Call `get_person_messages_range` with `name`, `after`, `before` dates, and `limit` of 300
2. Read through the returned messages carefully. Note:
   - Key quotes (copy them VERBATIM with exact timestamps)
   - Topics discussed, events mentioned, emotional tone shifts
   - Inside jokes, recurring themes, nicknames
3. Write the chapter for this period immediately while the messages are fresh
4. Move to the next period

You may also call `search_messages` to search for specific topics across the full timeline if a theme from one period seems relevant to others.

### Strict factual grounding rules

These rules are ABSOLUTE and cannot be violated:

- **Every fact must come from a message you actually read** via `get_person_messages_range` or `search_messages`
- **Every quote must be verbatim** — copied exactly from the tool output, with the exact timestamp shown
- **Never infer personality traits, pet details, hobbies, or facts** that are not explicitly stated in messages you read
- **Never guess what happened between messages** — if there's a gap, acknowledge it
- **If a period is sparse** (few messages), say so honestly — don't invent activity to fill the gap
- **Never fabricate dialogue** or combine parts of different messages into one quote
- **Attribute quotes correctly** — check sender name carefully (watch for "me" vs the other person)

## Phase 3.5: photo curation

If the user provides a photos directory, curate the best photos for the visualization. Photos are interspersed chronologically between sections, so select ones that complement the narrative.

### Finding the photos directory

Ask the user if they have a photos directory. Common locations:
- WhatsApp export: `~/Downloads/{person}_chat_new/` or similar
- iMessage: photos may need to be extracted via `download_media`
- Manual folder the user specifies

### Selection process

1. **List all images** in the directory using `Glob` (pattern: `*.jpg`, `*.jpeg`, `*.png`)
2. **Visually inspect candidates** using the `Read` tool on image files — Claude Code can see images
3. **Select 15-25 photos** based on the criteria below
4. **Note filenames with dates** — WhatsApp photos use `IMG-YYYYMMDD-WA####.jpg` format; these dates are automatically parsed for chronological alignment with chapters

### What makes a GOOD photo (select these)

- **Couple photos**: the two people together — selfies, posed shots, candid moments. HIGHEST PRIORITY.
- **Meaningful moments**: birthday celebrations, trips, dates, cooking together, holidays
- **Scenic/travel photos**: beautiful landscapes, landmarks, cityscapes from trips mentioned in messages
- **Pet photos**: if they have a pet discussed in messages, include 1-2
- **Food/restaurant photos**: if food is a shared interest, include 1-2 standout ones
- **Art/illustration**: any custom artwork or illustrations of the couple

### What makes a BAD photo (skip these)

- **Screenshots**: app screenshots, website captures, Twitter/Reddit posts, news articles
- **Memes/jokes**: funny images, reaction GIFs, internet humor
- **Product photos**: shopping screenshots, Amazon listings, apartment listings
- **Documents**: receipts, flight itineraries, forms, PDFs
- **Low quality**: blurry, very dark, or tiny images
- **Duplicate/similar**: if 5 photos are from the same event, pick the best 1-2

### Inspection strategy

Don't inspect all 100+ photos individually. Be efficient:
1. Read a batch of ~10 filenames at a time
2. Visually inspect the ones with promising names (e.g., dates matching trip periods, not "WA0001" early sequence numbers which tend to be forwarded memes)
3. Photos from dates that align with chapter periods are especially valuable
4. Larger file sizes often indicate real camera photos vs. forwarded memes (check with `ls -la`)

### Output

Build a JSON array of selected photo file paths under `OPENMESSAGES_EXPORT_DIR` (default `~/Documents/OpenMessage`). If the user intentionally wants to read photos from elsewhere, explain that `OPENMESSAGES_ALLOW_ANY_EXPORT_PATH=1` is required before passing those absolute paths to `render_story`.

## Phase 4: render

Assemble the Story JSON and call `render_story`. The JSON format:

```json
{
  "title": "A descriptive title for the relationship story",
  "summary": "A 2-3 sentence overview of the relationship arc",
  "chapters": [
    {
      "title": "Chapter title",
      "content": "The narrative paragraph(s) for this chapter. Write in a warm, reflective tone. Reference specific messages and moments you observed.",
      "period": "2023-2024",
      "quotes": [
        {
          "sender": "Exact sender name from messages",
          "text": "Exact verbatim quote from a message",
          "timestamp": "2024-01-15T14:30:00Z"
        }
      ]
    }
  ]
}
```

Call `render_story` with:
- `name`: the person's name
- `story_json`: the JSON string above
- `output_path`: `stories/{name_lowercase}_story.html` (written under `OPENMESSAGES_EXPORT_DIR`, default `~/Documents/OpenMessage`)
- `timezone`: "America/New_York" (or ask the user if unsure)
- `photo_paths`: JSON array of curated photo file paths from Phase 3.5. By default, these must be inside `OPENMESSAGES_EXPORT_DIR`; set `OPENMESSAGES_ALLOW_ANY_EXPORT_PATH=1` only for an explicit outside-directory export.

Photos are automatically sorted chronologically by date in their filename, and interspersed between sections so early photos appear near early chapters and recent photos near recent ones.

Include any style parameters the user specified (colors, password, etc).

## Phase 5: report

After rendering, report:
- Output file path and size
- Number of chapters and date range covered
- A brief summary of each chapter's theme
- Number of photos included and how they were selected
- Remind the user they can open it locally or deploy to Vercel

## Writing style guidelines

- Write in second person ("you" and the person's name) — this is their story to read
- Be warm and observational, not melodramatic
- Let the messages speak for themselves — your narrative connects and contextualizes
- Include 2-4 quotes per chapter (more for dense periods, fewer for sparse ones)
- Chapter titles should be evocative but grounded (e.g. "Late nights in January" not "The dawn of forever")
- Acknowledge uncertainty: "the messages suggest..." rather than "you felt..."
