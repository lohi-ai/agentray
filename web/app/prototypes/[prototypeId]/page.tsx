import { permanentRedirect } from 'next/navigation';

// Same id space as /plans/[testId] — a saved /prototypes/<id> link lands on
// the same experiment under its product route.
export default async function PrototypeRoute({ params }: { params: Promise<{ prototypeId: string }> }) {
  const { prototypeId } = await params;
  permanentRedirect(`/plans/${encodeURIComponent(prototypeId)}`);
}
