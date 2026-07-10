// Composer "rich message" encoding shared by the send path (encode) and the user
// bubble (decode). Both features — slash-invoked skills and file attachments —
// are FE-only: the conversation store carries a single `message` string and the
// model history is derived server-side, so everything the agent needs (the skill
// directive, the file contents) must live in that one string. Encoding it in a
// deterministic, parseable shape lets the user bubble render the same compact
// chips whether it's showing the just-sent local turn or the reloaded server
// projection — there is no separate attachment channel to fall out of sync.

import type { AgentSkill } from '@/lib/api';

// Caps keep an attachment from blowing past the model's context (and from dumping
// a megabyte into a chat bubble). Text-only by design — the runtime has no vision
// channel here, so binary files are rejected at read time rather than mangled.
export const MAX_ATTACHMENTS = 4;
export const MAX_ATTACHMENT_CHARS = 12_000;
const READABLE_EXT = new Set([
  'txt', 'md', 'mdx', 'csv', 'tsv', 'json', 'jsonl', 'log', 'yaml', 'yml', 'xml',
  'html', 'htm', 'css', 'js', 'jsx', 'ts', 'tsx', 'py', 'go', 'rs', 'java', 'rb',
  'php', 'sql', 'sh', 'bash', 'toml', 'ini', 'env', 'conf', 'tf', 'graphql', 'svg',
]);

export type Attachment = {
  id: string;
  name: string;
  text: string;
  truncated: boolean;
};

// slugify turns a skill's display name into the /slash token a user types. Slugs
// are the token's serialized value too, so the send-path parser can match them.
export function slugify(name: string): string {
  return name.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
}

// isReadableFile gates the attach flow to text-like files: a `text/*` MIME, a
// known structured type, or a known extension. Anything else (images, PDFs,
// office docs, archives) is skipped with a message — inlining their bytes as text
// would just feed the agent garbage.
export function isReadableFile(file: File): boolean {
  if (file.type.startsWith('text/')) return true;
  if (/(json|xml|csv|yaml|x-sh|javascript|typescript)/.test(file.type)) return true;
  const ext = file.name.split('.').pop()?.toLowerCase() ?? '';
  return READABLE_EXT.has(ext);
}

// readAttachment loads a text file into an Attachment, truncating past the char
// cap so one big CSV can't dominate the turn. Returns null for unreadable files.
export async function readAttachment(file: File): Promise<Attachment | null> {
  if (!isReadableFile(file)) return null;
  const raw = await file.text();
  const truncated = raw.length > MAX_ATTACHMENT_CHARS;
  return {
    id: `${file.name}-${file.size}-${file.lastModified}`,
    name: file.name,
    text: truncated ? raw.slice(0, MAX_ATTACHMENT_CHARS) : raw,
    truncated,
  };
}

// --- encode (send path) ---------------------------------------------------

const SKILL_DIRECTIVE = (name: string) => `Use your "${name}" skill.`;
// A fenced block per file. The opening line is matched verbatim by the decoder,
// and the ``` fence keeps the file body out of the model's instruction stream.
const fileBlock = (a: Attachment) =>
  `Attached file \`${a.name}\`:\n\`\`\`\n${a.text}${a.truncated ? '\n… (truncated)' : ''}\n\`\`\``;

// composeMessage builds the single string sent to (and displayed for) a turn:
// skill directives first (parsed out of the typed /slug tokens), then the user's
// prose, then the attachment blocks. `skills` is the current agent's skill set,
// used to resolve a typed /slug back to its proper name and to ignore stray
// slashes that aren't real commands.
export function composeMessage(input: string, attachments: Attachment[], skills: AgentSkill[]): string {
  const bySlug = new Map(skills.map((s) => [slugify(s.name), s.name] as const));
  const picked = new Map<string, string>(); // slug -> name, de-duped, first-seen order
  // Strip the /slug command tokens out of the prose and collect the ones that map
  // to a real skill. `\B/` so we only catch a slash that starts a token.
  const body = input
    .replace(/(^|\s)\/([a-z0-9-]+)/g, (whole, lead: string, slug: string) => {
      const name = bySlug.get(slug);
      if (!name) return whole; // not a command — leave the text untouched
      if (!picked.has(slug)) picked.set(slug, name);
      return lead; // drop the token, keep the surrounding whitespace
    })
    .trim();

  const directives = [...picked.values()].map(SKILL_DIRECTIVE);
  const blocks = attachments.map(fileBlock);
  return [directives.join('\n'), body, blocks.join('\n\n')].filter(Boolean).join('\n\n');
}

// --- decode (user bubble) -------------------------------------------------

export type ParsedMessage = {
  text: string; // the human prose, with directives + file blocks removed
  skills: string[]; // skill names the turn invoked
  files: string[]; // attached file names
};

const DIRECTIVE_RE = /^Use your "(.+?)" skill\.$/;
const FILE_BLOCK_RE = /Attached file `(.+?)`:\n```\n[\s\S]*?\n```/g;

// parseRichMessage reverses composeMessage so the user bubble can render the prose
// plus compact skill/file chips. It runs on whatever string the store holds, so a
// reloaded turn renders identically to the one just sent.
export function parseRichMessage(message: string): ParsedMessage {
  const files: string[] = [];
  let rest = message.replace(FILE_BLOCK_RE, (_m, name: string) => {
    files.push(name);
    return '';
  });

  const skills: string[] = [];
  const lines = rest.split('\n');
  let i = 0;
  while (i < lines.length) {
    const m = lines[i].match(DIRECTIVE_RE);
    if (!m) break;
    skills.push(m[1]);
    i++;
  }
  rest = lines.slice(i).join('\n');

  return { text: rest.trim(), skills, files };
}
