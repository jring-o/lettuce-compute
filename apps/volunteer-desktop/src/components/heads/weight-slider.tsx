import { useRef } from "react";
import { Slider } from "@/components/ui/slider";

/** The range every weight slider offers at least: 1 to 100. */
const BASE_MAX = 100;

interface WeightSliderProps {
  label: string;
  value: number;
  /** One line saying what the number means. */
  caption: string;
  onChange: (weight: number) => void;
}

/**
 * A head or leaf weight: its label and number, the slider, and a caption.
 *
 * The CLI accepts any positive weight, so the range reaches the largest value
 * this control has shown: a weight of 200 set there sits at 200, not pinned at
 * 100, and dragging it down does not shrink the range under the handle.
 */
export function WeightSlider({ label, value, caption, onChange }: WeightSliderProps) {
  const maxRef = useRef(BASE_MAX);
  maxRef.current = Math.max(maxRef.current, value);

  return (
    <div className="space-y-1">
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{label}</span>
        <span>{value}</span>
      </div>
      <Slider min={1} max={maxRef.current} value={value} onChange={onChange} />
      <p className="text-xs text-muted-foreground">{caption}</p>
    </div>
  );
}
