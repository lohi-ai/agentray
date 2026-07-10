import { redirect } from 'next/navigation';

// The daily home is the dashboard, not the agent list. A dashboard rewards a
// user for just showing up (it shows what happened overnight on load); the
// agent list is a configuration surface that lives at /agents.
export default function Home() {
  redirect('/dashboard');
}
