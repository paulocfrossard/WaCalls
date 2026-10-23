/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Optional bearer token forwarded to the WaCalls SDK (REST auth / SSE query). */
  readonly VITE_WACALLS_API_KEY?: string;
}
