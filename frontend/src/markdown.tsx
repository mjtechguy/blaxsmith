// Agent-written Markdown (interaction questions and their evidence) rendered
// as React elements, never as an HTML string. marked only tokenizes; this file
// decides what each token becomes, so raw HTML is dropped (never injected),
// images show their alt text (no remote loads), headings stay below the page's
// own outline, and links keep only http, https, and mailto targets.
import { Fragment, type ReactNode } from "react";
import { Lexer, type Token, type Tokens } from "marked";

const entities: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: "\"", apos: "'", nbsp: " " };
const decode = (text: string) => text.replace(/&(#x[\da-f]+|#\d+|[a-z]+);/gi, (whole, name: string) => {
  if (name[0] !== "#") return entities[name.toLowerCase()] ?? whole;
  const code = name[1] === "x" || name[1] === "X" ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10);
  return code > 0 && code <= 0x10ffff ? String.fromCodePoint(code) : whole;
});

// Absolute http(s) and mailto links only; everything else renders as text.
export function safeHref(href: string): string | null {
  try {
    const url = new URL(href.trim());
    return url.protocol === "http:" || url.protocol === "https:" || url.protocol === "mailto:" ? url.href : null;
  } catch {
    return null;
  }
}

function inline(tokens: Token[] | undefined, key: string): ReactNode[] {
  return (tokens ?? []).map((token, i) => {
    const k = `${key}.${i}`;
    switch (token.type) {
      case "strong": return <strong key={k}>{inline(token.tokens, k)}</strong>;
      case "em": return <em key={k}>{inline(token.tokens, k)}</em>;
      case "del": return <del key={k}>{inline(token.tokens, k)}</del>;
      case "codespan": return <code key={k}>{decode(token.text)}</code>;
      case "br": return <br key={k} />;
      case "link": {
        const href = safeHref((token as Tokens.Link).href);
        const body = inline(token.tokens, k);
        return href ? <a key={k} href={href} target="_blank" rel="noopener noreferrer">{body}</a> : <span key={k}>{body}</span>;
      }
      case "image": return (token as Tokens.Image).text ? <span key={k}>{decode((token as Tokens.Image).text)}</span> : null;
      case "html": return null;
      case "checkbox": return <span key={k} aria-hidden="true">{(token as Tokens.Checkbox).checked ? "☑ " : "☐ "}</span>;
      case "text": case "escape": return "tokens" in token && token.tokens?.length ? <span key={k}>{inline(token.tokens, k)}</span> : decode(token.text);
      default: return "text" in token && typeof token.text === "string" ? decode(token.text) : null;
    }
  });
}

function blocks(tokens: Token[], key: string): ReactNode[] {
  return tokens.map((token, i) => {
    const k = `${key}.${i}`;
    switch (token.type) {
      case "paragraph": return <p key={k}>{inline(token.tokens, k)}</p>;
      case "heading": return <p key={k} className="md-heading"><strong>{inline(token.tokens, k)}</strong></p>;
      case "code": return <pre key={k} className="code-block"><code>{(token as Tokens.Code).text}</code></pre>;
      case "blockquote": return <blockquote key={k}>{blocks((token as Tokens.Blockquote).tokens, k)}</blockquote>;
      case "hr": return <hr key={k} />;
      case "list": {
        const list = token as Tokens.List;
        const items = list.items.map((item, j) => <li key={`${k}.${j}`}>{blocks(item.tokens, `${k}.${j}`)}</li>);
        return list.ordered ? <ol key={k} start={typeof list.start === "number" && list.start !== 1 ? list.start : undefined}>{items}</ol> : <ul key={k}>{items}</ul>;
      }
      // Tables stay plain text: the UI draws every table with DataTable.
      case "table": return <pre key={k} className="code-block">{token.raw.trim()}</pre>;
      case "text": return "tokens" in token && token.tokens ? <Fragment key={k}>{inline(token.tokens, k)}</Fragment> : decode(token.text);
      case "checkbox": return inline([token], k);
      case "html": case "space": case "def": return null;
      default: return null;
    }
  });
}

export function Markdown({ text, className = "md" }: { text: string; className?: string }) {
  return <div className={className}>{blocks(Lexer.lex(text, { gfm: true, breaks: true }), "md")}</div>;
}
