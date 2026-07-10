'use client';

import { Check } from 'lucide-react';
import { useAgentGrants } from '@/modules/app/hooks';
import { Modal } from '@/modules/shared/components/modal';
import { Button } from '@/modules/shared/components/signal-primitives';

// AssignProductsDialog makes the workspace-owned → project-granted model
// tangible: an agent belongs to the company (workspace); here you assign it to
// the products (projects) it should work on. Assigning grants access; the home
// product cannot be un-assigned for a default agent (the API enforces that).
export function AssignProductsDialog({ agentID, agentName, onClose }: { agentID: string; agentName: string; onClose: () => void }) {
  const { projects, grantedProjectIDs, loading, busy, assign, unassign } = useAgentGrants(agentID);

  return (
    <Modal
      title={`Assign ${agentName} to products`}
      onClose={onClose}
      footer={<Button variant="primary" size="sm" onClick={onClose}>Done</Button>}
    >
      <p style={{ color: 'var(--muted-foreground)', fontSize: 12.5, marginTop: 0 }}>
        This agent is owned by your workspace. Assign it to the products it should work on — it can
        cover more than one.
      </p>
      {loading ? (
        <p style={{ fontSize: 12.5, color: 'var(--muted-foreground)' }}>Loading assignments…</p>
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
          {projects.map((p) => {
            const granted = grantedProjectIDs.has(p.id);
            return (
              <div key={p.id} style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '8px 10px', border: '1px solid var(--border)', borderRadius: 'var(--radius)' }}>
                <span style={{ flex: 1, fontSize: 13 }}>{p.name}</span>
                <Button
                  variant={granted ? 'outline' : 'primary'}
                  size="sm"
                  disabled={busy}
                  icon={granted ? <Check size={14} /> : undefined}
                  onClick={() => void (granted ? unassign(p.id) : assign(p.id))}
                >
                  {granted ? 'Assigned' : 'Assign'}
                </Button>
              </div>
            );
          })}
          {projects.length === 0 ? (
            <p style={{ fontSize: 12.5, color: 'var(--muted-foreground)' }}>No other products in this workspace yet.</p>
          ) : null}
        </div>
      )}
    </Modal>
  );
}
