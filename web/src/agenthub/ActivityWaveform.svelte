<script lang="ts">
  import { onMount } from "svelte";
  import {
    ACTIVITY_VISIBLE_MS,
    activityWaveformPatchRange,
    waveformSampleY,
  } from "./companion/model";

  let { pulses, live }: { pulses: any[]; live: boolean } = $props();
  let track: HTMLDivElement;
  let firstCanvas: HTMLCanvasElement;
  let secondCanvas: HTMLCanvasElement;
  let syncPulses = () => {};

  $effect(() => {
    pulses;
    syncPulses();
  });

  onMount(() => {
    type Tile = { canvas: HTMLCanvasElement; startTime: number };

    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
    let animation: Animation | null = null;
    let timer = 0;
    let disposed = false;
    let visibleWidth = 1;
    let height = 1;
    let cycleAnchorTime = Date.now();
    let cycleStartedAt = performance.now();
    let tiles: Tile[] = [];
    let knownPulses = new Set<string>();

    const pulseKey = (pulse: any) => `${pulse.sessionId}:${pulse.at}`;
    const stop = () => {
      window.clearTimeout(timer);
      timer = 0;
      animation?.cancel();
      animation = null;
    };

    const visibleRightTime = () => {
      const elapsed = animation?.currentTime == null
        ? performance.now() - cycleStartedAt
        : Number(animation.currentTime);
      return cycleAnchorTime + Math.max(0, Math.min(ACTIVITY_VISIBLE_MS, elapsed));
    };

    const drawTileRange = (tile: Tile, fromTime: number, toTime: number) => {
      const context = tile.canvas.getContext("2d");
      if (!context) return;
      const exactStart = Math.max(0, (fromTime - tile.startTime) / ACTIVITY_VISIBLE_MS * visibleWidth);
      const exactEnd = Math.min(visibleWidth, (toTime - tile.startTime) / ACTIVITY_VISIBLE_MS * visibleWidth);
      if (exactEnd <= exactStart) return;
      const sampleStart = Math.max(0, Math.floor(exactStart) - 6);
      const sampleEnd = Math.min(visibleWidth, Math.ceil(exactEnd) + 6);

      context.save();
      context.beginPath();
      context.rect(exactStart, 0, exactEnd - exactStart, height);
      context.clip();
      context.clearRect(exactStart, 0, exactEnd - exactStart, height);
      context.lineWidth = 1;
      context.strokeStyle = "#1c303c";
      context.beginPath();
      context.moveTo(sampleStart, height / 3);
      context.lineTo(sampleEnd, height / 3);
      context.moveTo(sampleStart, height * 2 / 3);
      context.lineTo(sampleEnd, height * 2 / 3);
      context.stroke();

      context.lineWidth = 1.9;
      context.lineJoin = "round";
      context.lineCap = "round";
      context.strokeStyle = "#62e6a5";
      context.shadowBlur = 12;
      context.shadowColor = "rgb(98 230 165 / 70%)";
      context.beginPath();
      let firstPoint = true;
      for (let x = sampleStart; x <= sampleEnd; x += 2) {
        const sampleTime = tile.startTime + x / visibleWidth * ACTIVITY_VISIBLE_MS;
        const y = (waveformSampleY as any)(pulses, sampleTime, height);
        if (firstPoint) context.moveTo(x, y);
        else context.lineTo(x, y);
        firstPoint = false;
      }
      context.stroke();
      context.restore();
    };

    const drawWholeTile = (tile: Tile) => {
      drawTileRange(tile, tile.startTime, tile.startTime + ACTIVITY_VISIBLE_MS);
    };

    const startAnimation = () => {
      cycleStartedAt = performance.now();
      if (reducedMotion.matches) {
        timer = window.setTimeout(() => rebuild(Date.now()), 1000);
        return;
      }
      animation = track.animate(
        [{ transform: "translateX(0)" }, { transform: `translateX(${-visibleWidth}px)` }],
        { duration: ACTIVITY_VISIBLE_MS, easing: "linear", fill: "forwards" },
      );
      animation.onfinish = rotateTiles;
    };

    const rotateTiles = () => {
      if (disposed || document.hidden || tiles.length !== 2) return;
      const [expired, visible] = tiles;
      animation?.cancel();
      animation = null;
      expired.startTime = visible.startTime + ACTIVITY_VISIBLE_MS;
      track.append(expired.canvas);
      tiles = [visible, expired];
      cycleAnchorTime = expired.startTime;
      drawWholeTile(expired);
      startAnimation();
    };

    const sizeCanvas = (canvas: HTMLCanvasElement, pixelRatio: number) => {
      canvas.width = Math.round(visibleWidth * pixelRatio);
      canvas.height = Math.round(height * pixelRatio);
      canvas.getContext("2d")?.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
    };

    const rebuild = (anchorTime = Date.now()) => {
      if (disposed || document.hidden) return;
      stop();
      const container = track.parentElement;
      if (!container) return;
      visibleWidth = Math.max(1, container.clientWidth);
      height = Math.max(1, container.clientHeight);
      const pixelRatio = Math.min(2, Math.max(1, window.devicePixelRatio || 1));
      sizeCanvas(firstCanvas, pixelRatio);
      sizeCanvas(secondCanvas, pixelRatio);
      track.append(firstCanvas, secondCanvas);
      tiles = [
        { canvas: firstCanvas, startTime: anchorTime - ACTIVITY_VISIBLE_MS },
        { canvas: secondCanvas, startTime: anchorTime },
      ];
      cycleAnchorTime = anchorTime;
      knownPulses = new Set((pulses || []).map(pulseKey));
      drawWholeTile(tiles[0]);
      drawWholeTile(tiles[1]);
      startAnimation();
    };

    const updatePulses = () => {
      if (disposed || document.hidden) return;
      if (reducedMotion.matches || !animation) {
        rebuild(Date.now());
        return;
      }
      const currentKeys = new Set((pulses || []).map(pulseKey));
      const added = (pulses || []).filter((pulse: any) => !knownPulses.has(pulseKey(pulse)));
      knownPulses = currentKeys;
      if (!added.length) return;

      const pixelFenceMs = ACTIVITY_VISIBLE_MS / visibleWidth * 2;
      const writeFence = visibleRightTime() + pixelFenceMs;
      for (const tile of tiles) {
        const ranges = added.flatMap((pulse: any) => {
          const range = (activityWaveformPatchRange as any)(
            pulse.at,
            writeFence,
            tile.startTime,
            tile.startTime + ACTIVITY_VISIBLE_MS,
          );
          return range ? [range] : [];
        }).sort((left: any, right: any) => left.from - right.from);
        const merged: Array<{ from: number; to: number }> = [];
        for (const range of ranges) {
          const previous = merged[merged.length - 1];
          if (previous && range.from <= previous.to) previous.to = Math.max(previous.to, range.to);
          else merged.push({ ...range });
        }
        for (const range of merged) drawTileRange(tile, range.from, range.to);
      }
    };

    const onVisibilityChange = () => {
      if (document.hidden) stop();
      else rebuild(Date.now());
    };
    const onMotionChange = () => rebuild(Date.now());
    const resize = new ResizeObserver(() => rebuild(Date.now()));
    resize.observe(track.parentElement!);
    reducedMotion.addEventListener("change", onMotionChange);
    document.addEventListener("visibilitychange", onVisibilityChange);
    syncPulses = updatePulses;
    rebuild();

    return () => {
      disposed = true;
      stop();
      syncPulses = () => {};
      resize.disconnect();
      reducedMotion.removeEventListener("change", onMotionChange);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  });
</script>

<div class="companion-ecg" data-real-pulse-count={pulses.length} role="img" aria-label="Live activity waveform">
  <div bind:this={track} class="companion-ecg-track" aria-hidden="true">
    <canvas bind:this={firstCanvas}></canvas>
    <canvas bind:this={secondCanvas}></canvas>
  </div>
  {#if live}<span class="companion-scanbar"></span>{/if}
</div>
