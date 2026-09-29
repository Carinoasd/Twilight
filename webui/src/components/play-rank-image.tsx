"use client";

import { useState } from "react";
import { Film, UserRound } from "lucide-react";
import { playRankImageUrl } from "@/lib/play-rank-image-url";
import { API_BASE } from "@/lib/api-request";
import { sanitizeImageUrl } from "@/lib/safe-url";

export function PlayRankImage({ src, kind }: { src?: string; kind: "poster" | "avatar" }) {
  const path = playRankImageUrl(src, kind);
  const safeSrc = path ? sanitizeImageUrl(`${API_BASE}${path}`) : undefined;
  // A changed source gets a fresh loading state after refresh or a board switch.
  return <RankImage key={`${kind}:${safeSrc ?? ""}`} src={safeSrc} kind={kind} />;
}

function RankImage({ src, kind }: { src?: string; kind: "poster" | "avatar" }) {
  const [failed, setFailed] = useState(false);
  const avatar = kind === "avatar";
  const Icon = avatar ? UserRound : Film;
  return (
    <div aria-hidden="true" className={avatar ? "w-9 shrink-0 sm:w-10" : "w-10 shrink-0 sm:w-12"}>
      {src && !failed ? (
        // Reuse the configured API origin and the browser's existing session.
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={src}
          alt=""
          loading="lazy"
          decoding="async"
          referrerPolicy="no-referrer"
          onError={() => setFailed(true)}
          className={avatar
            ? "aspect-square w-full rounded-full object-cover"
            : "block h-auto w-full rounded-md"}
        />
      ) : (
        <div data-rank-image-fallback={kind} className={`flex items-center justify-center bg-muted text-muted-foreground ${avatar ? "aspect-square rounded-full" : "aspect-[2/3] rounded-md"}`}>
          <Icon className="h-5 w-5" />
        </div>
      )}
    </div>
  );
}
