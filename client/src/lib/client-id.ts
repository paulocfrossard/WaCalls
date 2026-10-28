const KEY = "wacalls.clientId";

const generate = (): string => {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return "c-" + Array.from(bytes, (b) => b.toString(36)).join("") + Date.now().toString(36);
};

export const getClientId = (): string => {
  let id = localStorage.getItem(KEY);
  if (!id) {
    id = generate();
    localStorage.setItem(KEY, id);
  }
  return id;
};
