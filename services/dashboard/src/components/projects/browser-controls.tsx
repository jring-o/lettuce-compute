"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Search, X } from "lucide-react";
import { Input } from "@/components/ui/input";
import type { ResearchArea } from "@/lib/validations/project";

interface BrowserControlsProps {
  researchAreas: ResearchArea[];
  search: string;
  researchArea: string;
  sort: string;
  onSearchChange: (value: string) => void;
  onResearchAreaChange: (value: string) => void;
  onSortChange: (value: string) => void;
}

export function BrowserControls({
  researchAreas,
  search,
  researchArea,
  sort,
  onSearchChange,
  onResearchAreaChange,
  onSortChange,
}: BrowserControlsProps) {
  const [inputValue, setInputValue] = useState(search);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    setInputValue(search);
  }, [search]);

  const handleSearchInput = useCallback(
    (value: string) => {
      setInputValue(value);
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => {
        if (value.length === 0 || value.length >= 2) {
          onSearchChange(value);
        }
      }, 300);
    },
    [onSearchChange],
  );

  const clearSearch = useCallback(() => {
    setInputValue("");
    if (debounceRef.current) clearTimeout(debounceRef.current);
    onSearchChange("");
  }, [onSearchChange]);

  useEffect(() => {
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, []);

  return (
    <div
      data-testid="browser-controls"
      className="flex flex-col gap-3 sm:flex-row sm:items-center"
    >
      <div className="relative flex-1">
        <Search className="absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          type="text"
          placeholder="Search leafs..."
          value={inputValue}
          onChange={(e) => handleSearchInput(e.target.value)}
          className="pl-9 pr-8"
          data-testid="search-input"
        />
        {inputValue && (
          <button
            type="button"
            onClick={clearSearch}
            className="absolute right-2.5 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
            data-testid="clear-search"
            aria-label="Clear search"
          >
            <X className="size-4" />
          </button>
        )}
        {inputValue.length === 1 && (
          <p className="mt-1 text-xs text-muted-foreground" data-testid="search-hint">
            Type at least 2 characters to search
          </p>
        )}
      </div>

      <select
        value={researchArea}
        onChange={(e) => onResearchAreaChange(e.target.value)}
        className="h-8 rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
        data-testid="research-area-filter"
        aria-label="Filter by research area"
      >
        <option value="">All Research Areas</option>
        {researchAreas.map((area) => (
          <option key={area.id} value={area.slug}>
            {area.name}
          </option>
        ))}
      </select>

      <select
        value={sort}
        onChange={(e) => onSortChange(e.target.value)}
        className="h-8 rounded-lg border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
        data-testid="sort-select"
        aria-label="Sort leafs"
      >
        <option value="updated_at">Recently Active</option>
        <option value="created_at">Newest</option>
      </select>
    </div>
  );
}
