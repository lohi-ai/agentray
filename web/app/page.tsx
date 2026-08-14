import { redirect } from 'next/navigation';
import { signedInLandingTarget } from '@/lib/ia';

// Conversation is the front door. Saved views stay at /dashboard; a signed-in
// session should land in chat so the first action is asking, not scanning a board.
export default function Home() {
  redirect(signedInLandingTarget());
}
