import * as React from "react";
import { cn } from "@/lib/utils";

interface SliderProps {
  min: number;
  max: number;
  step?: number;
  value: number;
  onChange: (value: number) => void;
  className?: string;
  disabled?: boolean;
}

const Slider = React.forwardRef<HTMLInputElement, SliderProps>(
  ({ min, max, step = 1, value, onChange, className, disabled }, ref) => {
    return (
      <input
        ref={ref}
        type="range"
        min={min}
        max={max}
        step={step}
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
        disabled={disabled}
        className={cn(
          "w-full h-2 rounded-lg appearance-none cursor-pointer bg-secondary accent-primary disabled:cursor-not-allowed disabled:opacity-50",
          className
        )}
      />
    );
  }
);
Slider.displayName = "Slider";

export { Slider };
