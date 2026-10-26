import type { Metadata } from 'next';
import { NotFoundState } from '@/components/NotFoundState';

export const metadata: Metadata = {
  title: 'page not found | peasant',
};

// An address no route answers, reached by a client-side navigation or /404.
// It stays inside the local shell as fairtrade's empty state, in place of
// Next's default page, whose own style paints the body white under the app's
// light ink in the light theme.
export default function NotFound() {
  return <NotFoundState />;
}
