/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Optional WaCalls API origin; empty/undefined means same-origin. */
  readonly VITE_WACALLS_BASE_URL?: string;
  /** @deprecated Static API keys are no longer read by the client — the SDK
   *  authenticates with the session token from the auth store. */
  readonly VITE_WACALLS_API_KEY?: string;
}
