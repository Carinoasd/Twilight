import { useCallback, useRef } from "react";

// Retain a failed submission's key for an explicit retry. Editing its text or
// switching tickets starts a different submission. Success retires the key.
export function useTicketReplyKey() {
  const pending = useRef<{ ticketID: number; content: string; key: string } | null>(null);
  const keyFor = useCallback((ticketID: number, content: string) => {
    if (pending.current?.ticketID !== ticketID || pending.current.content !== content) {
      const bytes = crypto.getRandomValues(new Uint8Array(24));
      pending.current = { ticketID, content, key: Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("") };
    }
    return pending.current.key;
  }, []);
  const retire = useCallback((key: string) => {
    if (pending.current?.key === key) pending.current = null;
  }, []);
  return { keyFor, retire };
}
