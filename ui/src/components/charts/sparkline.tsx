// ─────────────────────────────────────────────────────────────────────────
//  Sparklines — word-sized, data-intense graphics (Tufte, Beautiful Evidence).
//  No axes, no gridlines, no decoration. Height ≈ x-height of the value beside
//  them; length ≈ a short phrase. The final point is marked with a single dot
//  so the eye lands on the current value. They show *shape* (macro) next to a
//  precise readout (micro); they are never meant to be read for exact values.
// ─────────────────────────────────────────────────────────────────────────

interface SparklineProps {
  data: number[];
  width?: number;
  height?: number;
  /** Primary trace color. */
  color?: string;
  /** Show a dot on the final point (the current value). Default true. */
  endDot?: boolean;
  /** Faint area beneath the line — a quiet second layer for magnitude. */
  area?: boolean;
  strokeWidth?: number;
  className?: string;
  ariaLabel?: string;
}

// Map a value series onto the drawable box, leaving a hair of padding so the
// stroke and the end dot never clip against the edges.
function project(
  data: number[],
  w: number,
  h: number,
  pad: number,
): Array<{ x: number; y: number }> {
  const n = data.length;
  if (n === 0) return [];
  const min = Math.min(...data);
  const max = Math.max(...data);
  const span = max - min || 1; // flat series -> draw a centered line
  const innerW = w - pad * 2;
  const innerH = h - pad * 2;
  return data.map((v, i) => ({
    x: pad + (n === 1 ? innerW / 2 : (i / (n - 1)) * innerW),
    y: pad + innerH - ((v - min) / span) * innerH,
  }));
}

/** A line/area sparkline. Best for continuous trends (cost, tokens). */
export function Sparkline({
  data,
  width = 96,
  height = 28,
  color = "hsl(198 90% 54%)",
  endDot = true,
  area = true,
  strokeWidth = 1.5,
  className,
  ariaLabel,
}: SparklineProps) {
  if (!data || data.length < 2) return <SparkEmpty width={width} height={height} className={className} />;

  const pad = 3;
  const pts = project(data, width, height, pad);
  const first = pts[0];
  const last = pts[pts.length - 1];
  if (!first || !last) return <SparkEmpty width={width} height={height} className={className} />;
  const line = pts.map((p) => `${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(" ");
  const areaPath =
    `M ${first.x.toFixed(1)},${(height - pad).toFixed(1)} ` +
    pts.map((p) => `L ${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(" ") +
    ` L ${last.x.toFixed(1)},${(height - pad).toFixed(1)} Z`;

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      role="img"
      aria-label={ariaLabel}
      preserveAspectRatio="none"
    >
      {area && <path d={areaPath} fill={color} fillOpacity={0.1} stroke="none" />}
      <polyline
        points={line}
        fill="none"
        stroke={color}
        strokeWidth={strokeWidth}
        strokeLinejoin="round"
        strokeLinecap="round"
      />
      {endDot && (
        <circle cx={last.x} cy={last.y} r={strokeWidth + 1} fill={color} />
      )}
    </svg>
  );
}

/** A bar sparkline. Best for discrete counts (requests per period). */
export function SparkBars({
  data,
  width = 96,
  height = 28,
  color = "hsl(198 90% 54%)",
  className,
  ariaLabel,
}: Omit<SparklineProps, "endDot" | "area" | "strokeWidth">) {
  if (!data || data.length < 1) return <SparkEmpty width={width} height={height} className={className} />;

  const pad = 2;
  const n = data.length;
  const max = Math.max(...data) || 1;
  const innerH = height - pad * 2;
  const slot = (width - pad * 2) / n;
  const bw = Math.max(1, slot * 0.62);

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      role="img"
      aria-label={ariaLabel}
      preserveAspectRatio="none"
    >
      {data.map((v, i) => {
        const bh = Math.max(1, (v / max) * innerH);
        const x = pad + i * slot + (slot - bw) / 2;
        const y = pad + innerH - bh;
        // The last bar is the current value — give it full weight, mute the rest.
        const isLast = i === n - 1;
        return (
          <rect
            key={i}
            x={x.toFixed(1)}
            y={y.toFixed(1)}
            width={bw.toFixed(1)}
            height={bh.toFixed(1)}
            rx={0.5}
            fill={color}
            fillOpacity={isLast ? 1 : 0.45}
          />
        );
      })}
    </svg>
  );
}

function SparkEmpty({
  width,
  height,
  className,
}: {
  width: number;
  height: number;
  className?: string;
}) {
  return (
    <svg width={width} height={height} className={className} aria-hidden>
      <line
        x1={2}
        y1={height / 2}
        x2={width - 2}
        y2={height / 2}
        stroke="currentColor"
        strokeOpacity={0.18}
        strokeDasharray="2 3"
      />
    </svg>
  );
}
