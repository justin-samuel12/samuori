type Kind = "logo" | "icon" | "stacked";
type Finish = "flat" | "3d";

const viewBox: Record<Kind, [number, number]> = {
  logo: [1200, 400],
  icon: [400, 420],
  stacked: [700, 650],
};

/**
 * The supplied Samuori artwork from public/brand/, unchanged. Both scheme variants render and CSS shows the one
 * matching the resolved theme (app.css, .samuori-brand-img). Use "flat" in app chrome and small sizes, "3d" only
 * for large displays. An empty alt marks a decorative repeat.
 */
export function BrandMark({
  kind = "icon",
  finish = "flat",
  alt = "Samuori",
  className = "w-8",
}: {
  kind?: Kind;
  finish?: Finish;
  alt?: string;
  className?: string;
}) {
  const [width, height] = viewBox[kind];
  return (
    <>
      {(["light", "dark"] as const).map((scheme) => (
        <img
          key={scheme}
          src={`/brand/samuori-${finish}-${kind}-${scheme}.svg`}
          alt={alt}
          width={width}
          height={height}
          className={`samuori-brand-img samuori-brand-img--${scheme} ${className}`}
        />
      ))}
    </>
  );
}
