// Host plugin code, outside the byte-identical Pi sources. This synchronous
// hook mirrors Tool.PrepareArguments before Pi performs its own validation.
const space = /^\p{White_Space}+|\p{White_Space}+$/gu;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

function bounded(value: string, limit: number): string {
  const bytes = encoder.encode(value);
  if (bytes.length <= limit) return value;
  while (limit > 0 && (bytes[limit]! & 0xc0) === 0x80) limit--;
  return decoder.decode(bytes.subarray(0, limit));
}

function fields(value: unknown): [string, unknown][] | undefined {
  if (value === null) return [];
  if (typeof value !== "object" || Array.isArray(value)) return undefined;
  // encoding/json matches struct fields case-insensitively (including long s).
  return Object.entries(value!).map(([key, field]) => [key.replace(/ſ/g, "s").toLowerCase(), field]);
}

type Option = { label: string; description?: string };

export function prepareAskArguments(raw: unknown): unknown {
  const input = fields(raw);
  if (!input) return raw;
  let question = "", multi = false;
  let options: Option[] = [];
  for (const [key, value] of input) {
    if (key === "question") {
      if (value === null) continue; // Go ignores null for an existing scalar.
      if (typeof value !== "string") return raw;
      question = value;
    } else if (key === "multi") {
      if (value === null) continue;
      if (typeof value !== "boolean") return raw;
      multi = value;
    } else if (key === "options") {
      options = [];
      if (value === null) continue;
      if (!Array.isArray(value)) return raw;
      // Decode every item before clamping, even items later overwritten by a
      // differently cased field: Go returns any decoding error in the object.
      for (const option of value) {
        const entry = fields(option);
        if (!entry) return raw;
        const decoded: Option = { label: "" };
        for (const [field, text] of entry) {
          if (field !== "label" && field !== "description") continue;
          if (text === null) continue;
          if (typeof text !== "string") return raw;
          decoded[field] = text;
        }
        options.push(decoded);
      }
    }
  }
  const normalized: { question: string; options?: Option[]; multi?: boolean } = {
    question: bounded(question.replace(space, ""), 2000),
  };
  if (options.length) normalized.options = options.slice(0, 8).map((option) => {
    const out: Option = { label: bounded(option.label.replace(space, ""), 200) };
    if (option.description) out.description = bounded(option.description, 200);
    return out;
  });
  if (multi) normalized.multi = true;
  return normalized;
}
