/** Bound one non-preemptible file message; slow acknowledgements reduce its size.
 * This favors terminal responsiveness over maximum bulk throughput on high RTT.
 */
export class TransferChunkSizer {
  size = 32 * 1024
  observe(elapsedMS: number): void {
    if (elapsedMS > 600) this.size = Math.max(8 * 1024, this.size / 2)
    else if (elapsedMS < 120) this.size = Math.min(64 * 1024, this.size * 2)
  }
}
