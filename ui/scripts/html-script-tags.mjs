export function withoutComments(html) {
  return html.replace(/<!--[\s\S]*?(?:-->|$)/g, "");
}

function parseAttributes(source) {
  const attributes = new Map();
  for (const match of source.matchAll(/([^\s=/>]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g)) {
    attributes.set(match[1].toLowerCase(), match[2] ?? match[3] ?? match[4] ?? "");
  }
  return attributes;
}

export function scriptTags(html) {
  return [...withoutComments(html).matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script\s*>/gi)].map((match) => ({
    attributes: parseAttributes(match[1]),
    body: match[2],
  }));
}
