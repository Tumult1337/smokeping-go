const COMMENT_OPEN = "<!--";
const COMMENT_CLOSERS = ["-->", "--!>"];
const TEXT_ONLY_ELEMENTS = new Set(["iframe", "noembed", "noframes", "noscript", "plaintext", "script", "style", "textarea", "title", "xmp"]);

const TAG_NAME = /[A-Za-z][^\s/>]*/y;
const ATTRIBUTE_GAP = /[\s/]*/y;
const ATTRIBUTE_NAME = /[^\s/>][^\s/>=]*/y;
const VALUE_START = /\s*=\s*/y;
const QUOTED_VALUE = /"([^"]*)"|'([^']*)'/y;
const UNQUOTED_VALUE = /[^\s>]*/y;

export function withoutComments(html) {
  return html.replace(/<!--[\s\S]*?(?:-->|$)/g, "");
}

function stickyMatch(pattern, html, position) {
  pattern.lastIndex = position;
  const match = pattern.exec(html);
  return match === null ? null : { match, end: pattern.lastIndex };
}

function commentEnd(html, open) {
  const bodyStart = open + COMMENT_OPEN.length;
  if (html.startsWith(">", bodyStart)) return bodyStart + 1;
  if (html.startsWith("->", bodyStart)) return bodyStart + 2;
  const ends = COMMENT_CLOSERS.map((closer) => {
    const at = html.indexOf(closer, bodyStart);
    return at === -1 ? html.length : at + closer.length;
  });
  return Math.min(...ends);
}

function readAttributeValue(html, position) {
  const valueStart = stickyMatch(VALUE_START, html, position);
  if (valueStart === null) return { value: "", end: position };
  const quoted = stickyMatch(QUOTED_VALUE, html, valueStart.end);
  if (quoted !== null) return { value: quoted.match[1] ?? quoted.match[2], end: quoted.end };
  const unquoted = stickyMatch(UNQUOTED_VALUE, html, valueStart.end);
  return { value: unquoted.match[0], end: unquoted.end };
}

function readStartTag(html, open) {
  const tagName = stickyMatch(TAG_NAME, html, open + 1);
  const attributes = new Map();
  let position = tagName.end;
  for (;;) {
    position = stickyMatch(ATTRIBUTE_GAP, html, position).end;
    if (position >= html.length) return { name: tagName.match[0].toLowerCase(), attributes, end: html.length };
    if (html[position] === ">") return { name: tagName.match[0].toLowerCase(), attributes, end: position + 1 };
    const attributeName = stickyMatch(ATTRIBUTE_NAME, html, position);
    const { value, end } = readAttributeValue(html, attributeName.end);
    const name = attributeName.match[0].toLowerCase();
    if (!attributes.has(name)) attributes.set(name, value);
    position = end;
  }
}

function textEnd(html, name, from) {
  const endTag = new RegExp(`</${name}[\\s/>]`, "gi");
  endTag.lastIndex = from;
  const match = endTag.exec(html);
  return match === null ? html.length : match.index;
}

function markupEnd(html, open) {
  const close = html.indexOf(">", open);
  return close === -1 ? html.length : close + 1;
}

export function startTags(html) {
  const tags = [];
  let position = 0;
  while (position < html.length) {
    const open = html.indexOf("<", position);
    if (open === -1) break;
    const next = html[open + 1] ?? "";
    if (html.startsWith(COMMENT_OPEN, open)) {
      position = commentEnd(html, open);
      continue;
    }
    if (/[A-Za-z]/.test(next)) {
      const tag = readStartTag(html, open);
      position = tag.end;
      if (TEXT_ONLY_ELEMENTS.has(tag.name)) {
        const bodyEnd = textEnd(html, tag.name, position);
        tags.push({ name: tag.name, attributes: tag.attributes, body: html.slice(position, bodyEnd) });
        position = bodyEnd;
        continue;
      }
      tags.push({ name: tag.name, attributes: tag.attributes, body: "" });
      continue;
    }
    position = next === "/" || next === "!" || next === "?" ? markupEnd(html, open) : open + 1;
  }
  return tags;
}

export function scriptTags(html) {
  return startTags(html)
    .filter((tag) => tag.name === "script")
    .map(({ attributes, body }) => ({ attributes, body }));
}
