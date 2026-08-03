import { describe, it, expect } from 'vitest';
import {
  ChannelTopic,
  AnnotationAxis,
  subscribe,
  subscriptionKey,
  acceptSubscription,
} from './messages';
import type { SubscriptionVisitor } from './messages';

// ---------------------------------------------------------------------------
// subscriptionKey
// ---------------------------------------------------------------------------

describe('subscriptionKey', () => {
  it('returns "dashboard" for dashboard subscription', () => {
    expect(subscriptionKey(subscribe.dashboard())).toBe('dashboard');
  });

  it('returns "sessions" for sessions subscription', () => {
    expect(subscriptionKey(subscribe.sessions())).toBe('sessions');
  });

  it('returns "trends" for trends subscription', () => {
    expect(subscriptionKey(subscribe.trends())).toBe('trends');
  });

  it('returns "quality" for quality subscription', () => {
    expect(subscriptionKey(subscribe.quality())).toBe('quality');
  });

  it('returns "session_detail:<id>" for sessionDetail subscription', () => {
    expect(subscriptionKey(subscribe.sessionDetail('sess-123'))).toBe('session_detail:sess-123');
  });

  it('returns "annotations:session:<id>" for annotations with Session axis', () => {
    expect(
      subscriptionKey(subscribe.annotations(AnnotationAxis.Session, 'sess-abc')),
    ).toBe('annotations:session:sess-abc');
  });

  it('returns "annotations:type:<id>" for annotations with Type axis', () => {
    expect(
      subscriptionKey(subscribe.annotations(AnnotationAxis.Type, 'quality.session_outcome')),
    ).toBe('annotations:type:quality.session_outcome');
  });

  it('returns "annotations:project:<id>" for annotations with Project axis', () => {
    expect(
      subscriptionKey(subscribe.annotations(AnnotationAxis.Project, 'proj-hash')),
    ).toBe('annotations:project:proj-hash');
  });

  it('returns "project_familiarity:<id>" for projectFamiliarity subscription', () => {
    expect(
      subscriptionKey(subscribe.projectFamiliarity('proj-hash')),
    ).toBe('project_familiarity:proj-hash');
  });
});

// ---------------------------------------------------------------------------
// subscribe factory — correct topic discriminant and payload shape
// ---------------------------------------------------------------------------

describe('subscribe factory', () => {
  it('dashboard() produces topic "dashboard"', () => {
    expect(subscribe.dashboard().topic).toBe(ChannelTopic.Dashboard);
  });

  it('sessions() produces topic "sessions"', () => {
    expect(subscribe.sessions().topic).toBe(ChannelTopic.Sessions);
  });

  it('trends() produces topic "trends"', () => {
    expect(subscribe.trends().topic).toBe(ChannelTopic.Trends);
  });

  it('quality() produces topic "quality"', () => {
    expect(subscribe.quality().topic).toBe(ChannelTopic.Quality);
  });

  it('sessionDetail() produces topic "session_detail" and carries id', () => {
    const sub = subscribe.sessionDetail('x');
    expect(sub.topic).toBe(ChannelTopic.SessionDetail);
    expect(sub.id).toBe('x');
  });

  it('sessionDetail() serializes "id" to match Go ChannelSubscription wire format', () => {
    const sub = subscribe.sessionDetail('agent-abc123');
    const wire = JSON.parse(JSON.stringify(sub));
    expect(wire).toEqual({ topic: 'session_detail', id: 'agent-abc123' });
    expect(wire).not.toHaveProperty('sessionId');
  });

  it('annotations() produces topic "annotations" and carries axis + id', () => {
    const sub = subscribe.annotations(AnnotationAxis.Session, 'my-id');
    expect(sub.topic).toBe(ChannelTopic.Annotations);
    expect(sub.axis).toBe(AnnotationAxis.Session);
    expect(sub.id).toBe('my-id');
  });

  it('annotations() with Type axis carries correct axis value', () => {
    const sub = subscribe.annotations(AnnotationAxis.Type, 'quality.session_outcome');
    expect(sub.axis).toBe(AnnotationAxis.Type);
    expect(sub.id).toBe('quality.session_outcome');
  });

  it('annotations() with Project axis carries correct axis value', () => {
    const sub = subscribe.annotations(AnnotationAxis.Project, 'proj-hash');
    expect(sub.axis).toBe(AnnotationAxis.Project);
    expect(sub.id).toBe('proj-hash');
  });

  it('projectFamiliarity() produces topic "project_familiarity" and carries id', () => {
    const sub = subscribe.projectFamiliarity('proj-hash');
    expect(sub.topic).toBe(ChannelTopic.ProjectFamiliarity);
    expect(sub.id).toBe('proj-hash');
  });
});

// ---------------------------------------------------------------------------
// acceptSubscription — visitor dispatch
// ---------------------------------------------------------------------------

/**
 * A visitor that returns the topic name string for each variant.
 * Used to verify that each subscription type dispatches to the correct method.
 */
const topicNameVisitor: SubscriptionVisitor<string> = {
  dashboard:            () => 'visited:dashboard',
  sessions:             () => 'visited:sessions',
  trends:               () => 'visited:trends',
  quality:              () => 'visited:quality',
  sessionDetail:        (s) => `visited:session_detail:${s.id}`,
  annotations:          (s) => `visited:annotations:${s.axis}:${s.id}`,
  projectFamiliarity:   (s) => `visited:project_familiarity:${s.id}`,
};

describe('acceptSubscription visitor dispatch', () => {
  it('dispatches dashboard subscription to dashboard() method', () => {
    expect(acceptSubscription(subscribe.dashboard(), topicNameVisitor)).toBe('visited:dashboard');
  });

  it('dispatches sessions subscription to sessions() method', () => {
    expect(acceptSubscription(subscribe.sessions(), topicNameVisitor)).toBe('visited:sessions');
  });

  it('dispatches trends subscription to trends() method', () => {
    expect(acceptSubscription(subscribe.trends(), topicNameVisitor)).toBe('visited:trends');
  });

  it('dispatches quality subscription to quality() method', () => {
    expect(acceptSubscription(subscribe.quality(), topicNameVisitor)).toBe('visited:quality');
  });

  it('dispatches sessionDetail subscription to sessionDetail() method with sessionId', () => {
    expect(
      acceptSubscription(subscribe.sessionDetail('sess-456'), topicNameVisitor),
    ).toBe('visited:session_detail:sess-456');
  });

  it('dispatches annotations(Session) subscription to annotations() method with axis and id', () => {
    expect(
      acceptSubscription(subscribe.annotations(AnnotationAxis.Session, 'sess-abc'), topicNameVisitor),
    ).toBe('visited:annotations:session:sess-abc');
  });

  it('dispatches annotations(Type) subscription to annotations() method with axis and id', () => {
    expect(
      acceptSubscription(
        subscribe.annotations(AnnotationAxis.Type, 'quality.session_outcome'),
        topicNameVisitor,
      ),
    ).toBe('visited:annotations:type:quality.session_outcome');
  });

  it('dispatches annotations(Project) subscription to annotations() method with axis and id', () => {
    expect(
      acceptSubscription(
        subscribe.annotations(AnnotationAxis.Project, 'proj-hash'),
        topicNameVisitor,
      ),
    ).toBe('visited:annotations:project:proj-hash');
  });

  it('dispatches projectFamiliarity subscription to projectFamiliarity() method with id', () => {
    expect(
      acceptSubscription(subscribe.projectFamiliarity('proj-hash'), topicNameVisitor),
    ).toBe('visited:project_familiarity:proj-hash');
  });

  it('visitor receives the full annotations subscription object (axis + id accessible)', () => {
    let capturedAxis: string | undefined;
    let capturedId: string | undefined;

    const capturingVisitor: SubscriptionVisitor<void> = {
      dashboard:            () => undefined,
      sessions:             () => undefined,
      trends:               () => undefined,
      quality:              () => undefined,
      sessionDetail:        () => undefined,
      annotations:          (s) => { capturedAxis = s.axis; capturedId = s.id; },
      projectFamiliarity:   () => undefined,
    };

    acceptSubscription(
      subscribe.annotations(AnnotationAxis.Project, 'proj-xyz'),
      capturingVisitor,
    );

    expect(capturedAxis).toBe(AnnotationAxis.Project);
    expect(capturedId).toBe('proj-xyz');
  });
});
