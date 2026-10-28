// The six bands of the flat mark in clockwise order; geometry from public/brand/samuori-flat-icon-*.svg.
const bands: ["n" | "t", string][] = [
  [
    "n",
    "M81 81 L207 7 Q223 -3 239 7 L324 55 L157 152 Q143 161 143 178 L143 191 L98 165 Q78 153 78 132 L78 105 Q78 90 81 81 Z",
  ],
  ["t", "M263 101 L323 66 Q338 58 352 66 L421 107 Q438 117 438 137 L438 202 Z"],
  [
    "n",
    "M240 121 Q256 112 271 121 L420 208 Q438 218 438 238 L438 325 Q438 344 421 354 L375 381 L375 307 Q375 284 356 273 L205 186 Q190 177 190 164 Q190 151 204 143 Z",
  ],
  [
    "t",
    "M303 252 L350 279 Q363 287 363 305 L363 371 Q363 389 346 399 L239 461 Q223 470 207 461 L115 408 L279 313 Q303 299 303 278 Z",
  ],
  ["n", "M4 244 L194 354 L119 397 Q105 405 91 397 L22 357 Q4 347 4 328 Z"],
  [
    "t",
    "M65 92 L22 117 Q4 128 4 147 L4 209 Q4 228 22 239 L181 331 Q196 340 211 331 L242 313 Q260 302 260 283 Q260 268 245 259 L88 168 Q65 155 65 132 Z",
  ],
];

/**
 * "weave" for inline and small waits, "assemble" for the first load. Reduced motion shows the still mark.
 * Set decorative when a surrounding element already announces the wait.
 */
export function Spinner({
  variant = "weave",
  size = 16,
  label = "Loading",
  decorative = false,
}: {
  variant?: "weave" | "assemble";
  size?: number;
  label?: string;
  decorative?: boolean;
}) {
  const a11y = decorative
    ? { "aria-hidden": true }
    : { role: "status", "aria-label": label };
  return (
    <span
      className={`samuori-mark-spinner samuori-mark-spinner--${variant}`}
      {...a11y}
    >
      <svg
        viewBox="0 0 400 420"
        width={size}
        height={Math.round((size * 420) / 400)}
        aria-hidden="true"
        focusable="false"
      >
        <g transform="translate(23 16) scale(.8)">
          {bands.map(([tone, d], i) => (
            <path
              key={i}
              className={`samuori-band-${tone} samuori-band-${i + 1}`}
              d={d}
            />
          ))}
        </g>
      </svg>
    </span>
  );
}
