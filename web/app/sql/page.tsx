import { SQLPage } from '@/modules/sql';

export default async function SQLRoute({ searchParams }: { searchParams: Promise<{ q?: string | string[] }> }) {
  const query = (await searchParams).q;
  return <SQLPage initialSQL={Array.isArray(query) ? query[0] : query} />;
}
