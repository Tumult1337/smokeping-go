import { useCallback, useEffect, useId, useRef, useState, type FocusEvent, type KeyboardEvent, type ReactNode } from "react";
import {
  decodeKeyInMode,
  edgeIndex,
  formatLocalTime,
  isKeyboardMode,
  isZoomed,
  keyHelpText,
  moveIndex,
  moveRow,
  nearestIndex,
  nextAnnouncement,
  pickAnnouncement,
  resolveZoomNow,
  shouldAnnounce,
  type Announcement,
  zoomAnnouncement,
  zoomIn,
  zoomOut,
  zoomReset,
  type ChartKind,
  type KeyAction,
  type KeyInput,
  type ZoomResult,
  type ZoomWindow,
} from "./chartA11y";

export interface ChartCursor {
  idx: number;
  sec: number;
  row: number | null;
}

interface Options {
  kind: ChartKind;
  name: string;
  times: readonly number[];
  rowCount?: number;
  fromSec?: number;
  toSec?: number;
  rangeSpanSec?: number;
  readout: (cursor: ChartCursor) => string;
  onPick?: (cursor: ChartCursor) => number | null;
  onZoomChange?: (window: ZoomWindow | null) => void;
}

interface FocusProps {
  role: "application";
  "aria-roledescription": string;
  "aria-label": string;
  "aria-describedby": string;
  tabIndex: 0;
  onKeyDown: (e: KeyboardEvent<HTMLElement>) => void;
  onKeyUp: (e: KeyboardEvent<HTMLElement>) => void;
  onFocus: (e: FocusEvent<HTMLElement>) => void;
  onBlur: () => void;
}

export interface ChartKeyboard {
  focusProps: FocusProps;
  focusClassName: string;
  cursor: ChartCursor | null;
  keyboardMode: boolean;
  onPointerLeave: () => void;
  liveRegion: ReactNode;
  keyHelp: ReactNode;
}

function keyInput(e: KeyboardEvent<HTMLElement>): KeyInput {
  return {
    key: e.key,
    shiftKey: e.shiftKey,
    ctrlKey: e.ctrlKey,
    altKey: e.altKey,
    metaKey: e.metaKey,
    repeat: e.repeat,
    isComposing: e.nativeEvent.isComposing,
  };
}

function matchesFocusVisible(el: HTMLElement): boolean {
  try {
    return el.matches(":focus-visible");
  } catch {
    return false;
  }
}

export function useChartKeyboard(options: Options): ChartKeyboard {
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const helpId = useId();
  const cursorRef = useRef<ChartCursor | null>(null);
  const repeatedRef = useRef(false);
  const [cursor, setCursor] = useState<ChartCursor | null>(null);
  const focusVisibleRef = useRef(false);
  const [announcement, setAnnouncement] = useState<Announcement>({ text: "", seq: 0 });
  const [focusVisible, setFocusVisible] = useState(false);

  const announce = useCallback((text: string) => {
    setAnnouncement((previous) => nextAnnouncement(previous, text));
  }, []);

  const commitCursor = useCallback((next: ChartCursor | null) => {
    cursorRef.current = next;
    setCursor(next);
  }, []);

  const applyCursorAction = useCallback(
    (action: KeyAction, current: ChartCursor): ChartCursor | null => {
      const { times, rowCount = 0 } = optionsRef.current;
      const count = times.length;
      const row = current.row;
      let idx = Math.min(current.idx, count - 1);
      let nextRow = row;
      if (action.type === "step") {
        const moved = moveIndex(idx, count, action.direction, action.large);
        if (moved == null) return null;
        idx = moved;
      } else if (action.type === "edge") {
        const edge = edgeIndex(count, action.edge);
        if (edge == null) return null;
        idx = edge;
      } else if (action.type === "row") {
        nextRow = moveRow(row, rowCount, action.direction);
      }
      return { idx, sec: times[idx], row: nextRow };
    },
    [],
  );

  const runZoom = useCallback((action: KeyAction, current: ChartCursor) => {
    const { fromSec, toSec, rangeSpanSec, onZoomChange } = optionsRef.current;
    if (fromSec == null || toSec == null || rangeSpanSec == null || !onZoomChange) return;
    const window: ZoomWindow = { from: fromSec, to: toSec };
    let result: ZoomResult = { changed: false };
    if (action.type === "zoomIn") result = zoomIn(window, current.sec);
    else if (action.type === "zoomOut") result = zoomOut(window, current.sec, rangeSpanSec, resolveZoomNow(Date.now() / 1000, toSec));
    else if (action.type === "zoomReset" && isZoomed(window, rangeSpanSec)) result = zoomReset(window, rangeSpanSec);
    if (!result.changed) return;
    onZoomChange(result.window);
    announce(zoomAnnouncement(result.window));
  }, [announce]);

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLElement>) => {
      const { kind, times, rowCount = 0, readout, onPick } = optionsRef.current;
      const input = keyInput(e);
      const keyboardMode = isKeyboardMode(cursorRef.current != null, focusVisibleRef.current);
      const action = decodeKeyInMode(input, kind, keyboardMode);
      if (!action) {
        if (input.repeat && decodeKeyInMode({ ...input, repeat: false }, kind, keyboardMode)) e.preventDefault();
        return;
      }
      e.preventDefault();
      if (e.repeat) repeatedRef.current = true;
      else repeatedRef.current = false;
      if (action.type === "escape") {
        commitCursor(null);
        announce("");
        return;
      }
      if (times.length === 0) return;
      const current = cursorRef.current;
      if (!current) {
        const first: ChartCursor = {
          idx: times.length - 1,
          sec: times[times.length - 1],
          row: kind === "heatmap" ? moveRow(null, rowCount, -1) : null,
        };
        commitCursor(first);
        announce(readout(first));
        return;
      }
      if (action.type === "pick") {
        const picked = onPick?.(current);
        if (picked != null) announce(pickAnnouncement(formatLocalTime(picked)));
        return;
      }
      if (action.type === "zoomIn" || action.type === "zoomOut" || action.type === "zoomReset") {
        runZoom(action, current);
        return;
      }
      const next = applyCursorAction(action, current);
      if (!next) return;
      commitCursor(next);
      if (shouldAnnounce("down", e.repeat, repeatedRef.current)) announce(readout(next));
    },
    [announce, applyCursorAction, commitCursor, runZoom],
  );

  const onKeyUp = useCallback((e: KeyboardEvent<HTMLElement>) => {
    const { kind, readout } = optionsRef.current;
    const current = cursorRef.current;
    const action = decodeKeyInMode({ ...keyInput(e), repeat: false }, kind, isKeyboardMode(current != null, focusVisibleRef.current));
    if (!action || (action.type !== "step" && action.type !== "row")) return;
    const repeated = repeatedRef.current;
    repeatedRef.current = false;
    if (current && shouldAnnounce("up", false, repeated)) announce(readout(current));
  }, [announce]);

  const onFocus = useCallback((e: FocusEvent<HTMLElement>) => {
    focusVisibleRef.current = matchesFocusVisible(e.currentTarget);
    setFocusVisible(focusVisibleRef.current);
  }, []);

  const onBlur = useCallback(() => {
    repeatedRef.current = false;
    focusVisibleRef.current = false;
    setFocusVisible(false);
    commitCursor(null);
    announce("");
  }, [announce, commitCursor]);

  const onPointerLeave = useCallback(() => {
    const current = cursorRef.current;
    if (current) setCursor({ ...current });
  }, []);

  const { times, rowCount, fromSec, toSec } = options;
  useEffect(() => {
    const current = cursorRef.current;
    if (!current || times.length === 0) return;
    if (fromSec != null && toSec != null && (current.sec < fromSec || current.sec > toSec)) {
      commitCursor(null);
      return;
    }
    const idx = times[current.idx] === current.sec ? current.idx : nearestIndex(times, current.sec);
    if (idx == null) return;
    const row = current.row == null ? null : Math.min(current.row, Math.max(0, (rowCount ?? 0) - 1));
    if (idx === current.idx && times[idx] === current.sec && row === current.row) return;
    commitCursor({ idx, sec: times[idx], row });
  }, [times, rowCount, fromSec, toSec, commitCursor]);

  const liveRegion = (
    <div className="sr-only" aria-live="polite" aria-atomic="true">
      {announcement.text && <span key={announcement.seq}>{announcement.text}</span>}
    </div>
  );
  const keyboardMode = isKeyboardMode(cursor != null, focusVisible);
  const keyHelp = (
    <div id={helpId} className={keyboardMode ? "chart-key-help" : "sr-only"}>
      {keyHelpText(options.kind)}
    </div>
  );

  return {
    focusProps: {
      role: "application",
      "aria-roledescription": "interactive chart",
      "aria-label": options.name,
      "aria-describedby": helpId,
      tabIndex: 0,
      onKeyDown,
      onKeyUp,
      onFocus,
      onBlur,
    },
    focusClassName: "chart-focus",
    cursor,
    keyboardMode,
    onPointerLeave,
    liveRegion,
    keyHelp,
  };
}
