/**
 * The step rail's labels, judged from boxes the browser measured. A label wider
 * than its marker, centred on it, leaves the frame at either end of the rail; a
 * frame that clips overflow then cuts the first and last label short. The page
 * is probed for each label's box, the box of its text, the frame's box and the
 * viewport, and this file says what is wrong with them, so the judgement can be
 * checked against boxes written down in a fixture without a browser.
 */

/** Sub-pixel layout can put an edge a fraction of a pixel out; that is not a spill. */
const TOLERANCE = 0.5;

/**
 * @typedef {{ text: string, place: string, left: number, right: number, textLeft: number, textRight: number, visible: boolean }} RailLabel
 * @typedef {{ labelled: boolean, frame: { left: number, right: number }, viewportWidth: number, labels: RailLabel[] }} Rail
 */

/**
 * What is wrong with a rail's labels; an empty list means each one sits inside
 * the frame and the viewport, the first starts the rail, the last ends it, and
 * none meets its neighbour. A rail that shows no labels has nothing to judge.
 *
 * @param {Rail} rail
 * @returns {string[]}
 */
export function railProblems(rail) {
  if (!rail.labelled) return [];
  const { frame, viewportWidth, labels } = rail;
  if (labels.length === 0) return ['the rail shows labels but none was found'];
  const problems = [];
  for (const label of labels) {
    const name = JSON.stringify(label.text);
    if (!label.visible) problems.push(`${name} is not visible`);
    const left = Math.min(label.left, label.textLeft);
    const right = Math.max(label.right, label.textRight);
    if (left < frame.left - TOLERANCE) problems.push(`${name} spills out of the frame on the left by ${(frame.left - left).toFixed(1)}px`);
    if (right > frame.right + TOLERANCE) problems.push(`${name} spills out of the frame on the right by ${(right - frame.right).toFixed(1)}px`);
    if (left < -TOLERANCE || right > viewportWidth + TOLERANCE) problems.push(`${name} leaves the viewport`);
  }
  if (labels.length > 1) {
    if (labels[0].place !== 'start') problems.push(`the first label is placed "${labels[0].place}", not "start"`);
    if (labels[labels.length - 1].place !== 'end') problems.push(`the last label is placed "${labels[labels.length - 1].place}", not "end"`);
  }
  const ordered = [...labels].sort((a, b) => a.left - b.left);
  for (let index = 1; index < ordered.length; index += 1) {
    const overlap = ordered[index - 1].right - ordered[index].left;
    if (overlap > TOLERANCE) problems.push(`${JSON.stringify(ordered[index - 1].text)} and ${JSON.stringify(ordered[index].text)} overlap by ${overlap.toFixed(1)}px`);
  }
  return problems;
}
