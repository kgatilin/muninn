// The six reads the server offers. Every failure is an Error carrying what
// the server said.

async function get(path, params) {
  const url = new URL(path, location.href);
  for (const [k, v] of Object.entries(params ?? {})) {
    if (Array.isArray(v)) v.forEach((x) => url.searchParams.append(k, x));
    else if (v !== undefined && v !== "" && v !== false) url.searchParams.set(k, v);
  }
  const res = await fetch(url);
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
  return body;
}

const of = (bank) => `api/banks/${encodeURIComponent(bank)}`;

export const banks = () => get("api/banks").then((r) => r.banks);
export const graph = (bank, { limit, withIds, hide, around }) => get(`${of(bank)}/graph`, { limit, with: withIds, hide, around });
export const node = (bank, id) => get(`${of(bank)}/node`, { id });
export const nodes = (bank, { kind, q }) => get(`${of(bank)}/nodes`, { kind, q });
export const search = (bank, { q, k, nograph, kind }) => get(`${of(bank)}/search`, { q, k, nograph: nograph ? 1 : undefined, kind });
export const usage = (bank) => get(`${of(bank)}/usage`);
