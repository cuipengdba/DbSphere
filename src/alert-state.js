export function nextAlertState(rule, signal, previous, observedAt) {
  const now = new Date(observedAt).getTime();
  if (!Number.isFinite(now)) throw new Error('Invalid alert observation time');
  if (signal.status !== 'ok') {
    if (previous?.status === 'firing') {
      return { next: { ...previous, missingSince: previous.missingSince ?? observedAt },
        transition: previous.missingSince ? null : 'signal_missing' };
    }
    return { next: null, transition: null };
  }
  const breach = rule.operator === '>=' ? signal.value >= rule.threshold
    : rule.operator === '>' ? signal.value > rule.threshold
      : rule.operator === '<=' ? signal.value <= rule.threshold
        : signal.value < rule.threshold;
  if (!breach) return { next: null, transition: previous?.status === 'firing' ? 'recovered' : null };
  const pendingSince = previous?.pendingSince ?? observedAt;
  const elapsed = Math.max(0, now - new Date(pendingSince).getTime());
  const firing = previous?.status === 'firing' || elapsed >= rule.durationSeconds * 1000;
  return { next: { status: firing ? 'firing' : 'pending', pendingSince,
    firedAt: previous?.firedAt ?? (firing ? observedAt : null),
    eventId: previous?.eventId ?? null, missingSince: null, lastValue: signal.value,
    lastObservedAt: observedAt }, transition: firing && previous?.status !== 'firing' ? 'firing' : null };
}
