/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Optional bearer token forwarded to the WaCalls SDK (REST auth / SSE query). */
  readonly VITE_WACALLS_API_KEY?: string;
  /** Optional API base URL (path or absolute origin). Empty = same-origin. */
  readonly VITE_WACALLS_BASE_URL?: string;
}
